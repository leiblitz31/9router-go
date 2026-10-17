// browser_chromedp.go — chromedp-backed browser automation for the farm
// modules (webshare hunter, grok farm, zai farm), replacing the Python
// DrissionPage drivers.
//
// One shared core (chromedpBrowser) implements all three driver seams:
//   - WebshareBrowser (+ WebshareBrowserFactory via ChromedpWebshareFactory)
//   - GrokBrowserDriver (via NewGrokChromedpDriver, matching GrokBrowserFactory)
//   - ZaiBrowserDriver (via NewZaiChromedpDriver)
//
// Requires a real Chromium on PATH at runtime (chromedp resolves it via
// ExecPath discovery); unit tests never launch a browser.

package petaniproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/samber/lo"
)

// Compile-time seam conformance.
var (
	_ WebshareBrowser        = (*chromedpBrowser)(nil)
	_ WebshareElement        = (*chromedpElement)(nil)
	_ WebshareBrowserFactory = ChromedpWebshareFactory{}
	_ GrokBrowserDriver      = (*chromedpGrokDriver)(nil)
	_ GrokBrowserFactory     = NewGrokChromedpDriver
	_ ZaiBrowserDriver       = (*chromedpZaiDriver)(nil)
)

// ---------------------------------------------------------------------------
// Launch configuration
// ---------------------------------------------------------------------------

// chromedpLaunchConfig is the plain-data launch spec for one browser.
// Kept separate from chromedp.ExecAllocatorOption (opaque funcs) so option
// building stays unit-testable without a browser.
type chromedpLaunchConfig struct {
	Headless   bool
	Args       []string // extra chromium flags, e.g. --window-size=1920,1080
	UserAgent  string   // "" = Chromium default
	Proxy      string   // --proxy-server value, "" for none
	Extensions []string // --load-extension dirs
	ExecPath   string   // "" = chromedp auto-discovery
}

// chromiumFlags renders the raw flag list for the config, in stable order.
// Pure and offline-testable; allocatorOpts maps these onto chromedp options.
func (c chromedpLaunchConfig) chromiumFlags() []string {
	var flags []string
	if c.Headless {
		flags = append(flags, "--headless", "--hide-scrollbars", "--mute-audio")
	}
	flags = append(flags, c.Args...)
	if c.UserAgent != "" && !hasChromiumFlag(flags, "--user-agent") {
		flags = append(flags, "--user-agent="+c.UserAgent)
	}
	if c.Proxy != "" && !hasChromiumFlag(flags, "--proxy-server") {
		flags = append(flags, "--proxy-server="+c.Proxy)
	}
	for _, ext := range c.Extensions {
		if ext != "" {
			flags = append(flags, "--load-extension="+ext)
		}
	}
	if c.ExecPath != "" {
		flags = append(flags, "--exec-path="+c.ExecPath)
	}
	return flags
}

// allocatorOpts converts the config to chromedp ExecAllocator options.
func (c chromedpLaunchConfig) allocatorOpts() []chromedp.ExecAllocatorOption {
	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
	}
	if c.ExecPath != "" {
		opts = append(opts, chromedp.ExecPath(c.ExecPath))
	}
	if c.Headless {
		opts = append(opts, chromedp.Headless)
	}
	if c.UserAgent != "" {
		opts = append(opts, chromedp.UserAgent(c.UserAgent))
	}
	if c.Proxy != "" {
		opts = append(opts, chromedp.ProxyServer(c.Proxy))
	}
	for _, f := range c.Args {
		name, value := splitChromiumFlag(f)
		opts = append(opts, chromedp.Flag(name, value))
	}
	for _, ext := range c.Extensions {
		if ext != "" {
			opts = append(opts, chromedp.Flag("load-extension", ext))
		}
	}
	return opts
}

// hasChromiumFlag reports whether any flag starts with the given prefix
// ("--proxy-server" matches "--proxy-server=http://...").
func hasChromiumFlag(flags []string, prefix string) bool {
	for _, f := range flags {
		if f == prefix || strings.HasPrefix(f, prefix+"=") {
			return true
		}
	}
	return false
}

// splitChromiumFlag turns "--name=value" into ("name", "value") and
// "--flag" into ("flag", true).
func splitChromiumFlag(f string) (string, any) {
	f = strings.TrimPrefix(f, "--")
	if i := strings.IndexByte(f, '='); i >= 0 {
		return f[:i], f[i+1:]
	}
	return f, true
}

// ---------------------------------------------------------------------------
// Human-like typing
// ---------------------------------------------------------------------------

// Human typing bounds: small random pause between keystrokes, mirroring the
// per-character element.input(ch) loop of the Python drivers.
const (
	chromedpTypePauseMin = 30 * time.Millisecond
	chromedpTypePauseMax = 80 * time.Millisecond
)

// humanTypePause draws one pause in [min,max]. Pure; the RNG is injected so
// tests can seed it deterministically.
func humanTypePause(rnd *rand.Rand, min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rnd.Int63n(int64(max-min)))
}

// ptrBool is lo.ToPtr aliased for the CDP *bool params.
var ptrBool = lo.ToPtr[bool]

// ---------------------------------------------------------------------------
// Shared browser core
// ---------------------------------------------------------------------------

// chromedpBrowser is the shared chromedp core behind all three driver seams.
// One instance owns one Chromium process (its own ExecAllocator): closing it
// quits the browser, matching browser.quit() semantics of the Python drivers.
type chromedpBrowser struct {
	ctx         context.Context // tab context (child of the allocator context)
	tabCancel   context.CancelFunc
	allocCancel context.CancelFunc // kills the whole browser process

	rnd          *rand.Rand
	typePauseMin time.Duration
	typePauseMax time.Duration
}

// newChromedpBrowser launches one Chromium process for cfg. The browser does
// not start until the first CDP call (chromedp lazily allocates on Run/Call).
func newChromedpBrowser(parentCtx context.Context, cfg chromedpLaunchConfig) (*chromedpBrowser, error) {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(parentCtx, cfg.allocatorOpts()...)
	tabCtx, tabCancel := chromedp.NewContext(allocCtx)
	return &chromedpBrowser{
		ctx:          tabCtx,
		tabCancel:    tabCancel,
		allocCancel:  allocCancel,
		rnd:          rand.New(rand.NewSource(time.Now().UnixNano())),
		typePauseMin: chromedpTypePauseMin,
		typePauseMax: chromedpTypePauseMax,
	}, nil
}

// withCtx merges the caller's cancellation into the browser tab context.
// chromedp actions require the tab context (it carries the Target); the
// caller-supplied ctx only contributes cancellation/deadline.
func (b *chromedpBrowser) withCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	merged, cancel := context.WithCancel(b.ctx)
	if ctx.Done() == nil {
		return merged, cancel
	}
	// Fast path: caller already gave up — propagate synchronously so the
	// merged context is done before we return (no goroutine needed).
	select {
	case <-ctx.Done():
		cancel()
		return merged, cancel
	default:
	}
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-merged.Done():
		}
	}()
	return merged, cancel
}

// errChromedpTargetMissing is returned when the chromedp target was never
// attached (no successful CDP call yet) or the context is not a chromedp one.
var errChromedpTargetMissing = errors.New("petaniproxy: chromedp target not initialized")

// Navigate loads url in the current tab (page.get).
func (b *chromedpBrowser) Navigate(ctx context.Context, url string) error {
	mctx, cancel := b.withCtx(ctx)
	defer cancel()
	if err := chromedp.Do(mctx, chromedp.Navigate(url)); err != nil {
		return fmt.Errorf("petaniproxy: navigate %s: %w", url, err)
	}
	return nil
}

// URL returns the current tab URL via target.GetTargetInfo.
func (b *chromedpBrowser) URL(ctx context.Context) (string, error) {
	c := chromedp.FromContext(b.ctx)
	if c == nil || c.Target == nil {
		return "", errChromedpTargetMissing
	}
	mctx, cancel := b.withCtx(ctx)
	defer cancel()
	res, err := chromedp.CallBrowser(mctx, target.GetTargetInfo,
		target.GetTargetInfoParams{TargetID: c.Target.TargetID})
	if err != nil {
		return "", fmt.Errorf("petaniproxy: get target info: %w", err)
	}
	if res.TargetInfo == nil {
		return "", errors.New("petaniproxy: empty target info")
	}
	return res.TargetInfo.URL, nil
}

// evalAny runs script in the main frame and returns the JSON-decoded result.
// JS undefined/null map to (nil, nil), mirroring page.run_js -> None.
func (b *chromedpBrowser) evalAny(ctx context.Context, script string) (any, error) {
	mctx, cancel := b.withCtx(ctx)
	defer cancel()
	v, err := chromedp.Run[any](mctx,
		chromedp.Evaluate[any](script, chromedp.EvalAwaitPromise))
	if err != nil {
		if errors.Is(err, chromedp.ErrJSUndefined) || errors.Is(err, chromedp.ErrJSNull) {
			return nil, nil
		}
		return nil, err
	}
	return v, nil
}

// Eval runs JS in the main frame (page.run_js).
func (b *chromedpBrowser) Eval(ctx context.Context, script string) (any, error) {
	return b.evalAny(ctx, script)
}

// EvalInFrame runs JS inside one frame (frame.run_js) via an isolated world
// bound to the frame id.
func (b *chromedpBrowser) EvalInFrame(ctx context.Context, frameID string, script string) (any, error) {
	mctx, cancel := b.withCtx(ctx)
	defer cancel()
	iso, err := chromedp.Call(mctx, page.CreateIsolatedWorld,
		page.CreateIsolatedWorldParams{
			FrameID:   cdp.FrameID(frameID),
			WorldName: "petaniproxy",
		})
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: create isolated world for frame %s: %w", frameID, err)
	}
	res, err := chromedp.Call(mctx, runtime.Evaluate, runtime.EvaluateParams{
		Expression:    script,
		ContextID:     iso.ExecutionContextID,
		ReturnByValue: ptrBool(true),
		AwaitPromise:  ptrBool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: evaluate in frame %s: %w", frameID, err)
	}
	if res.ExceptionDetails != nil {
		return nil, fmt.Errorf("petaniproxy: frame JS exception: %s",
			res.ExceptionDetails.Text)
	}
	if res.Result == nil || len(res.Result.Value) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(res.Result.Value, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// Frames lists frame ids of the current tab (page.get_frames).
func (b *chromedpBrowser) Frames(ctx context.Context) ([]string, error) {
	mctx, cancel := b.withCtx(ctx)
	defer cancel()
	res, err := chromedp.Call(mctx, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: get frame tree: %w", err)
	}
	var ids []string
	collectFrameIDs(res.FrameTree, &ids)
	return ids, nil
}

func collectFrameIDs(ft *page.FrameTree, out *[]string) {
	if ft == nil {
		return
	}
	if ft.Frame != nil {
		*out = append(*out, string(ft.Frame.ID))
	}
	for _, child := range ft.ChildFrames {
		collectFrameIDs(child, out)
	}
}

// DispatchMouseMove sends Input.dispatchMouseEvent mouseMoved (used by the
// Bezier mouse simulation).
func (b *chromedpBrowser) DispatchMouseMove(ctx context.Context, x, y int) error {
	mctx, cancel := b.withCtx(ctx)
	defer cancel()
	_, err := chromedp.Call(mctx, input.DispatchMouseEvent, input.DispatchMouseEventParams{
		Type: input.DispatchMouseEventTypeMouseMoved,
		X:    float64(x),
		Y:    float64(y),
	})
	if err != nil {
		return fmt.Errorf("petaniproxy: dispatch mouse move to %d,%d: %w", x, y, err)
	}
	return nil
}

// FindElement locates one element; ErrWebshareElementNotFound when absent.
func (b *chromedpBrowser) FindElement(ctx context.Context, selector string) (WebshareElement, error) {
	mctx, cancel := b.withCtx(ctx)
	defer cancel()
	query, isXPath := translateWebshareSelector(selector)
	var nodeID cdp.NodeID
	if isXPath {
		found, id, err := b.searchXPath(mctx, query)
		if err != nil {
			return nil, fmt.Errorf("petaniproxy: find element %q: %w", selector, err)
		}
		if !found {
			return nil, ErrWebshareElementNotFound
		}
		nodeID = id
	} else {
		doc, err := chromedp.Call(mctx, dom.GetDocument, dom.GetDocumentParams{})
		if err != nil {
			return nil, fmt.Errorf("petaniproxy: find element %q: %w", selector, err)
		}
		if doc.Root == nil {
			return nil, errors.New("petaniproxy: empty document")
		}
		q, err := chromedp.Call(mctx, dom.QuerySelector, dom.QuerySelectorParams{
			NodeID:   doc.Root.NodeID,
			Selector: query,
		})
		if err != nil {
			return nil, fmt.Errorf("petaniproxy: find element %q: %w", selector, err)
		}
		if q.NodeID == 0 {
			return nil, ErrWebshareElementNotFound
		}
		nodeID = q.NodeID
	}
	return &chromedpElement{browser: b, nodeID: nodeID}, nil
}

// searchXPath resolves an XPath via DOM.performSearch (Chrome treats queries
// starting with "//" as XPath) and returns the first matching node.
func (b *chromedpBrowser) searchXPath(ctx context.Context, xpath string) (bool, cdp.NodeID, error) {
	s, err := chromedp.Call(ctx, dom.PerformSearch, dom.PerformSearchParams{Query: xpath})
	if err != nil {
		return false, 0, err
	}
	defer func() {
		_, _ = chromedp.Call(ctx, dom.DiscardSearchResults, dom.DiscardSearchResultsParams{SearchID: s.SearchID})
	}()
	if s.ResultCount == 0 {
		return false, 0, nil
	}
	r, err := chromedp.Call(ctx, dom.GetSearchResults, dom.GetSearchResultsParams{
		SearchID:  s.SearchID,
		FromIndex: 0,
		ToIndex:   1,
	})
	if err != nil {
		return false, 0, err
	}
	if len(r.NodeIDs) == 0 {
		return false, 0, nil
	}
	return true, r.NodeIDs[0], nil
}

// Close quits the browser (browser.quit): releases the tab and kills the
// Chromium process via the allocator cancel.
func (b *chromedpBrowser) Close() error {
	if b.tabCancel != nil {
		b.tabCancel()
	}
	if b.allocCancel != nil {
		b.allocCancel()
	}
	return nil
}

// ---------------------------------------------------------------------------
// DrissionPage selector translation
// ---------------------------------------------------------------------------

// translateWebshareSelector converts a DrissionPage-style selector into a CSS
// selector or an XPath expression. It returns the translated query and whether
// it is XPath (true) or CSS (false).
//
// Supported forms (mirroring webshare_hunter.py usage):
//
//	"@name=email"              -> `[name="email"]`
//	"@type=email"              -> `[type="email"]`
//	"tag:input@type=checkbox"  -> `input[type="checkbox"]`
//	".PrivateSwitchBase-input" -> as-is
//	"text:Sign Up With Email"  -> `//*[contains(.,'Sign Up With Email')]`
//	"tag:button@@text():Download" -> `//button[contains(.,'Download')]`
func translateWebshareSelector(sel string) (query string, isXPath bool) {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return "", false
	}
	tag := ""
	var attrConds []string
	var textConds []string
	for _, part := range strings.Split(sel, "@@") {
		for _, tok := range strings.Split(part, "@") {
			switch {
			case strings.HasPrefix(tok, "tag:"):
				tag = strings.TrimPrefix(tok, "tag:")
			case strings.HasPrefix(tok, "text():"):
				textConds = append(textConds, xpathQuote(strings.TrimPrefix(tok, "text():")))
			case strings.HasPrefix(tok, "text:"):
				textConds = append(textConds, xpathQuote(strings.TrimPrefix(tok, "text:")))
			case strings.Contains(tok, "="):
				kv := strings.SplitN(tok, "=", 2)
				name := strings.TrimSpace(kv[0])
				val := strings.Trim(strings.TrimSpace(kv[1]), `"'`)
				if name != "" {
					attrConds = append(attrConds, fmt.Sprintf(`[%s="%s"]`, name, val))
				}
			case tok != "":
				// Bare token: treat as a CSS fragment (class, id, ...).
				attrConds = append(attrConds, tok)
			}
		}
	}
	if len(textConds) > 0 {
		node := tag
		if node == "" {
			node = "*"
		}
		var preds []string
		preds = append(preds, attrCondsToXPathPreds(attrConds)...)
		for _, t := range textConds {
			preds = append(preds, "contains(.,"+t+")")
		}
		return "//" + node + "[" + strings.Join(preds, " and ") + "]", true
	}
	var sb strings.Builder
	sb.WriteString(tag)
	for _, c := range attrConds {
		if strings.HasPrefix(c, "[") || strings.HasPrefix(c, ".") || strings.HasPrefix(c, "#") {
			sb.WriteString(c)
		} else {
			sb.WriteString(c)
		}
	}
	return sb.String(), false
}

// attrCondsToXPathPreds converts CSS-ish attribute conditions to XPath
// predicates; class/id fragments are kept as-is (rare in text queries).
func attrCondsToXPathPreds(conds []string) []string {
	var preds []string
	for _, c := range conds {
		c = strings.TrimSpace(c)
		if strings.HasPrefix(c, "[") && strings.HasSuffix(c, "]") {
			inner := strings.TrimSuffix(strings.TrimPrefix(c, "["), "]")
			if i := strings.IndexByte(inner, '='); i >= 0 {
				name := strings.TrimSpace(inner[:i])
				val := strings.Trim(strings.TrimSpace(inner[i+1:]), `"`)
				preds = append(preds, "@"+name+"="+xpathQuote(val))
			}
		}
	}
	return preds
}

// xpathQuote quotes a string for XPath, handling embedded single quotes via
// concat().
func xpathQuote(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	parts := strings.Split(s, "'")
	var b strings.Builder
	b.WriteString("concat(")
	for i, p := range parts {
		if i > 0 {
			b.WriteString(`, "'", `)
		}
		b.WriteString("'")
		b.WriteString(p)
		b.WriteString("'")
	}
	b.WriteString(")")
	return b.String()
}

// ---------------------------------------------------------------------------
// Element
// ---------------------------------------------------------------------------

// chromedpElement is a located DOM node (node id valid until navigation).
type chromedpElement struct {
	browser *chromedpBrowser
	nodeID  cdp.NodeID
}

// Click scrolls the element into view and dispatches a real mouse
// press/release at its center (element.click()).
func (e *chromedpElement) Click(ctx context.Context) error {
	r, ok := e.Rect(ctx)
	if !ok {
		return errors.New("petaniproxy: cannot click element without a box model")
	}
	cx, cy := rectCenter(r)
	mctx, cancel := e.browser.withCtx(ctx)
	defer cancel()
	if _, err := chromedp.Call(mctx, dom.ScrollIntoViewIfNeeded,
		dom.ScrollIntoViewIfNeededParams{NodeID: e.nodeID}); err != nil {
		return fmt.Errorf("petaniproxy: scroll element into view: %w", err)
	}
	for _, step := range []input.DispatchMouseEventParams{
		{Type: input.DispatchMouseEventTypeMouseMoved, X: cx, Y: cy},
		{Type: input.DispatchMouseEventTypeMousePressed, X: cx, Y: cy,
			Button: input.MouseButtonLeft, ClickCount: 1},
		{Type: input.DispatchMouseEventTypeMouseReleased, X: cx, Y: cy,
			Button: input.MouseButtonLeft, ClickCount: 1},
	} {
		if _, err := chromedp.Call(mctx, input.DispatchMouseEvent, step); err != nil {
			return fmt.Errorf("petaniproxy: click dispatch %s: %w", step.Type, err)
		}
	}
	return nil
}

// Rect returns the element's bounding box from DOM.getBoxModel
// (rect.location / rect.size). ok=false when no box model is available.
func (e *chromedpElement) Rect(ctx context.Context) (WebshareRect, bool) {
	mctx, cancel := e.browser.withCtx(ctx)
	defer cancel()
	res, err := chromedp.Call(mctx, dom.GetBoxModel,
		dom.GetBoxModelParams{NodeID: e.nodeID})
	if err != nil || res.Model == nil {
		return WebshareRect{}, false
	}
	return quadToWebshareRect(res.Model.Content), true
}

// quadToWebshareRect converts a CDP content quad [x1,y1,...x4,y4] to a rect.
func quadToWebshareRect(q dom.Quad) WebshareRect {
	if len(q) < 8 {
		return WebshareRect{}
	}
	minX, maxX := q[0], q[0]
	minY, maxY := q[1], q[1]
	for i := 2; i < 8; i += 2 {
		if q[i] < minX {
			minX = q[i]
		}
		if q[i] > maxX {
			maxX = q[i]
		}
		if q[i+1] < minY {
			minY = q[i+1]
		}
		if q[i+1] > maxY {
			maxY = q[i+1]
		}
	}
	return WebshareRect{X: minX, Y: minY, W: maxX - minX, H: maxY - minY}
}

// rectCenter returns the center point of a rect.
func rectCenter(r WebshareRect) (x, y float64) {
	return r.X + r.W/2, r.Y + r.H/2
}

// elementClearJS clears an input/textarea/contenteditable and fires
// input+change so framework bindings (React/Vue) pick it up.
const elementClearJS = `function(){
	if ('value' in this) { this.value = ''; }
	if (this.isContentEditable) { this.innerText = ''; }
	this.dispatchEvent(new Event('input', {bubbles: true}));
	this.dispatchEvent(new Event('change', {bubbles: true}));
	return true;
}`

// Clear focuses the element, selects all and clears it (element.clear()).
func (e *chromedpElement) Clear(ctx context.Context) error {
	mctx, cancel := e.browser.withCtx(ctx)
	defer cancel()
	resolved, err := chromedp.Call(mctx, dom.ResolveNode,
		dom.ResolveNodeParams{NodeID: e.nodeID})
	if err != nil {
		return fmt.Errorf("petaniproxy: resolve element node: %w", err)
	}
	if resolved.Object == nil || resolved.Object.ObjectID == "" {
		return errors.New("petaniproxy: cannot resolve element to a JS object")
	}
	_, err = chromedp.Call(mctx, runtime.CallFunctionOn, runtime.CallFunctionOnParams{
		ObjectID:            resolved.Object.ObjectID,
		FunctionDeclaration: elementClearJS,
		ReturnByValue:       ptrBool(true),
	})
	if err != nil {
		return fmt.Errorf("petaniproxy: clear element: %w", err)
	}
	return nil
}

// TypeChar types one character with a human-like pause (element.input(ch)).
func (e *chromedpElement) TypeChar(ctx context.Context, ch string) error {
	if ch == "" {
		return nil
	}
	mctx, cancel := e.browser.withCtx(ctx)
	defer cancel()
	if _, err := chromedp.Call(mctx, dom.Focus,
		dom.FocusParams{NodeID: e.nodeID}); err != nil {
		return fmt.Errorf("petaniproxy: focus element for typing: %w", err)
	}
	if _, err := chromedp.Call(mctx, input.InsertText,
		input.InsertTextParams{Text: ch}); err != nil {
		return fmt.Errorf("petaniproxy: insert text %q: %w", ch, err)
	}
	time.Sleep(humanTypePause(e.browser.rnd, e.browser.typePauseMin, e.browser.typePauseMax))
	return nil
}

// typeTextHuman focuses the node, clears it, then types text rune by rune
// with human-like pauses between keystrokes.
func (b *chromedpBrowser) typeTextHuman(ctx context.Context, nodeID cdp.NodeID, text string) error {
	el := &chromedpElement{browser: b, nodeID: nodeID}
	if err := el.Clear(ctx); err != nil {
		return err
	}
	for _, r := range text {
		if err := el.TypeChar(ctx, string(r)); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Webshare factory
// ---------------------------------------------------------------------------

// ChromedpWebshareFactory creates one chromedp browser per hunted account,
// mirroring the Python's ChromiumOptions: auto port, headless flag +
// WebshareHeadlessUserAgent + --window-size=1920,1080 (or --start-maximized),
// always WebshareDefaultChromiumArgs.
type ChromedpWebshareFactory struct{}

// NewBrowser implements WebshareBrowserFactory.
func (ChromedpWebshareFactory) NewBrowser(ctx context.Context, headless bool) (WebshareBrowser, error) {
	cfg := chromedpLaunchConfig{
		Headless: headless,
		Args:     append([]string{}, WebshareDefaultChromiumArgs...),
	}
	if headless {
		cfg.UserAgent = WebshareHeadlessUserAgent
		cfg.Args = append(cfg.Args, "--window-size=1920,1080")
	} else {
		cfg.Args = append(cfg.Args, "--start-maximized")
	}
	return newChromedpBrowser(ctx, cfg)
}

// ---------------------------------------------------------------------------
// Grok driver
// ---------------------------------------------------------------------------

// chromedpGrokDriver adapts the shared browser core to GrokBrowserDriver.
type chromedpGrokDriver struct {
	*chromedpBrowser
}

// NewGrokChromedpDriver launches one signup browser from GrokBrowserOptions.
// Its signature matches GrokBrowserFactory, so it can be wired directly.
func NewGrokChromedpDriver(ctx context.Context, opts GrokBrowserOptions) (GrokBrowserDriver, error) {
	cfg := chromedpLaunchConfig{
		Headless:   opts.Headless,
		Args:       append([]string{}, opts.ExtraArgs...),
		UserAgent:  opts.UserAgent,
		Proxy:      opts.Proxy,
		Extensions: append([]string{}, opts.Extensions...),
	}
	b, err := newChromedpBrowser(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &chromedpGrokDriver{b}, nil
}

// wrapRunJSScript wraps a page.run_js-style script so `arguments[i]` works:
// the JSON-encoded args are applied to an inner function holding the script.
func wrapRunJSScript(script string, args []any) (string, error) {
	enc, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("petaniproxy: cannot encode RunJS args: %w", err)
	}
	if len(args) == 0 {
		enc = []byte("[]")
	}
	return fmt.Sprintf("(function(){ var __args = %s; return (function(){ %s }).apply(null, __args); })()",
		string(enc), script), nil
}

// RunJS executes a script with arguments (page.run_js) and returns the
// script's return value.
func (d *chromedpGrokDriver) RunJS(ctx context.Context, script string, args ...any) (any, error) {
	wrapped, err := wrapRunJSScript(script, args)
	if err != nil {
		return nil, err
	}
	return d.evalAny(ctx, wrapped)
}

// grokCookieFromCDP converts one CDP cookie to the farm's GrokCookie.
func grokCookieFromCDP(c *network.Cookie) GrokCookie {
	g := GrokCookie{
		Name:     c.Name,
		Value:    c.Value,
		Domain:   c.Domain,
		Path:     c.Path,
		Secure:   c.Secure,
		HTTPOnly: c.HTTPOnly,
	}
	if c.Expires > 0 {
		g.Expires = int64(c.Expires)
	}
	return g
}

// Cookies returns the session cookies (page.cookies).
func (d *chromedpGrokDriver) Cookies(ctx context.Context) ([]GrokCookie, error) {
	mctx, cancel := d.withCtx(ctx)
	defer cancel()
	res, err := chromedp.Call(mctx, network.GetCookies, network.GetCookiesParams{})
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: grok get cookies: %w", err)
	}
	out := make([]GrokCookie, 0, len(res.Cookies))
	for _, c := range res.Cookies {
		if c == nil {
			continue
		}
		out = append(out, grokCookieFromCDP(c))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Zai driver
// ---------------------------------------------------------------------------

// ZaiChromedpOptions describes how to launch the Zai signup browser.
type ZaiChromedpOptions struct {
	Headless  bool
	Proxy     string // --proxy-server value, "" for none
	UserAgent string
	ExtraArgs []string // extra chromium flags
}

// chromedpZaiDriver adapts the shared browser core to ZaiBrowserDriver.
type chromedpZaiDriver struct {
	*chromedpBrowser
}

// NewZaiChromedpDriver launches one browser for a Zai registration attempt.
func NewZaiChromedpDriver(ctx context.Context, opts ZaiChromedpOptions) (ZaiBrowserDriver, error) {
	cfg := chromedpLaunchConfig{
		Headless:  opts.Headless,
		Args:      append([]string{}, opts.ExtraArgs...),
		UserAgent: opts.UserAgent,
		Proxy:     opts.Proxy,
	}
	b, err := newChromedpBrowser(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &chromedpZaiDriver{b}, nil
}

// resolveXPathNode returns the first node matching xpath (found=false when
// none matches, mirroring Python's `if name_input:` None guards).
func (d *chromedpZaiDriver) resolveXPathNode(ctx context.Context, xpath string) (cdp.NodeID, bool, error) {
	mctx, cancel := d.withCtx(ctx)
	defer cancel()
	found, id, err := d.searchXPath(mctx, xpath)
	if err != nil {
		return 0, false, err
	}
	return id, found, nil
}

// Fill types value into the first element matching xpath.
func (d *chromedpZaiDriver) Fill(ctx context.Context, xpath, value string) (bool, error) {
	id, found, err := d.resolveXPathNode(ctx, xpath)
	if err != nil || !found {
		return found, err
	}
	if err := d.typeTextHuman(ctx, id, value); err != nil {
		return true, err
	}
	return true, nil
}

// Click clicks the first element matching xpath.
func (d *chromedpZaiDriver) Click(ctx context.Context, xpath string) (bool, error) {
	id, found, err := d.resolveXPathNode(ctx, xpath)
	if err != nil || !found {
		return found, err
	}
	el := &chromedpElement{browser: d.chromedpBrowser, nodeID: id}
	if err := el.Click(ctx); err != nil {
		return true, err
	}
	return true, nil
}

// Has reports whether any element matches xpath.
func (d *chromedpZaiDriver) Has(ctx context.Context, xpath string) (bool, error) {
	_, found, err := d.resolveXPathNode(ctx, xpath)
	return found, err
}

// Cookies returns name->value cookies visible to the current tab.
func (d *chromedpZaiDriver) Cookies(ctx context.Context) (map[string]string, error) {
	mctx, cancel := d.withCtx(ctx)
	defer cancel()
	res, err := chromedp.Call(mctx, network.GetCookies, network.GetCookiesParams{})
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: zai get cookies: %w", err)
	}
	out := make(map[string]string, len(res.Cookies))
	for _, c := range res.Cookies {
		if c == nil {
			continue
		}
		out[c.Name] = c.Value
	}
	return out, nil
}

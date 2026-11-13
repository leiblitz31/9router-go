package executor

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
)

// The tool-output scanner: one incremental parser shared by the streaming
// and collect paths, so both give the same answer for the same text (the
// wrapper corpus showed stream/whole divergence is its own bug class).
//
// Fail-closed rules (the ds-web lesson of 2026-10-28, hardened):
//   - Marker text NEVER reaches the client. A span that begins like a tool
//     call and fails validation is dropped whole — trigger, payload and
//     closer — never released as prose, and never "un-stripped" back into
//     content when the remainder happens to parse as something else.
//   - The tool name comes only from the payload's "name" key, never from
//     decoration on the marker, and must be one of the tools the client
//     declared for this request. An undeclared name is a dropped span.
//   - Arguments must canonicalise to a JSON object. A string arguments
//     value is unwrapped one level; anything else broken is a dropped
//     span, not a tool_call with junk arguments.
//   - Orphan closers and model-faked [TOOL RESULT …] blocks in the answer
//     are protocol debris and are swallowed.
//   - A call whose payload never balances is dropped at the span cap or
//     at end of stream — counted in dropped, never emitted.
//
// The one deliberate release: a trigger followed (within the payload
// window) by a balanced JSON object that carries no top-level "name"
// key is ordinary prose that happens to follow a quoted marker — the
// object is released as text, the marker is not. An object WITH a name
// key is a call attempt: valid calls are emitted, broken ones dropped.

const (
	qwenWebMarkerMax    = 96 // longest marker span the classifier will read
	qwenWebPayloadWin   = 128
	qwenWebToolSpanMax  = 1 << 20
	qwenWebDebrisMax    = 8
	qwenWebResultBlkMax = 64 << 10
)

const (
	qwenMarkerNone = iota
	qwenMarkerOpen
	qwenMarkerClose
	qwenMarkerResultOpen
	qwenMarkerResultClose
)

// qwenWebToolCall is one validated call recovered from the answer.
type qwenWebToolCall struct {
	Name      string
	Arguments string // canonical compact JSON object
}

type qwenWebToolScanner struct {
	allowed map[string]bool
	buf     string

	inCall     bool
	seekStage  bool // in call, still hunting the payload '{'
	tailStage  bool // payload consumed; eating the closer/debris tail
	fenceArmed bool // the payload was wrapped in a ``` fence
	calls      []qwenWebToolCall
	dropped    int

	// Code-fence machine over emitted prose (a marker inside a fenced
	// example is documentation, not a call). Fed only with prose that is
	// actually released, in order; call spans never touch it.
	inFence bool
	linePos int // 0 = only blanks so far on this line, 1 = in content
	btRun   int // consecutive backticks at line start
	btArmed bool
}

func newQwenWebToolScanner(allowed map[string]bool) *qwenWebToolScanner {
	return &qwenWebToolScanner{allowed: allowed, linePos: 0}
}

// feed consumes one answer delta and returns the prose released by it and
// any calls completed during it.
func (s *qwenWebToolScanner) feed(chunk string) (string, []qwenWebToolCall) {
	s.buf += chunk
	before := len(s.calls)
	prose := s.process(false)
	return prose, s.calls[before:]
}

// finish flushes the scanner at end of stream.
func (s *qwenWebToolScanner) finish() (string, []qwenWebToolCall) {
	before := len(s.calls)
	prose := s.process(true)
	return prose, s.calls[before:]
}

// qwenWebParseToolOutput is the collect-path entry: feed everything, then
// finish. dropped counts spans swallowed by the fail-closed rules.
func qwenWebParseToolOutput(text string, allowed map[string]bool) (prose string, calls []qwenWebToolCall, dropped int) {
	sc := newQwenWebToolScanner(allowed)
	p1, c1 := sc.feed(text)
	p2, c2 := sc.finish()
	return p1 + p2, append(c1, c2...), sc.dropped
}

// qwenWebToolRuntime is the execute path's tool state for one turn: the
// scanner plus every validated call in order (its index in calls is the
// OpenAI tool_calls index) and the cleaned prose. The runtime is also
// the runaway guard (the wrappers' measured cap): a turn admits at most
// qwenWebMaxCallsPerTurn calls, and an exact duplicate of a call already
// admitted this turn is dropped — a model narrating the same call in a
// loop must not fan out into dozens of identical executions.
type qwenWebToolRuntime struct {
	scanner *qwenWebToolScanner
	calls   []qwenWebToolCall
	prose   strings.Builder
	seen    map[string]bool
	dropped int
}

const qwenWebMaxCallsPerTurn = 24

func newQwenWebToolRuntime(allowed map[string]bool) *qwenWebToolRuntime {
	return &qwenWebToolRuntime{scanner: newQwenWebToolScanner(allowed), seen: map[string]bool{}}
}

// admit filters one batch of freshly validated calls through the
// runaway guard and returns the ones this turn may actually emit.
func (r *qwenWebToolRuntime) admit(batch []qwenWebToolCall) []qwenWebToolCall {
	kept := batch[:0]
	for _, call := range batch {
		key := call.Name + "\x00" + call.Arguments
		if r.seen[key] || len(r.calls)+len(kept) >= qwenWebMaxCallsPerTurn {
			r.dropped++
			continue
		}
		r.seen[key] = true
		kept = append(kept, call)
	}
	return kept
}

// feed runs one answer delta through the scanner, accumulating the
// cleaned prose and returning the calls this delta completed (their
// indices start at base, captured before the call).
func (r *qwenWebToolRuntime) feed(chunk string) (prose string, newCalls []qwenWebToolCall, base int) {
	base = len(r.calls)
	prose, fresh := r.scanner.feed(chunk)
	newCalls = r.admit(fresh)
	r.calls = append(r.calls, newCalls...)
	r.prose.WriteString(prose)
	return prose, newCalls, base
}

// finish flushes the scanner at end of stream, same contract as feed.
func (r *qwenWebToolRuntime) finish() (prose string, newCalls []qwenWebToolCall, base int) {
	base = len(r.calls)
	prose, fresh := r.scanner.finish()
	newCalls = r.admit(fresh)
	r.calls = append(r.calls, newCalls...)
	r.prose.WriteString(prose)
	return prose, newCalls, base
}

func (s *qwenWebToolScanner) process(final bool) string {
	var out strings.Builder
	for {
		if s.inCall {
			if !s.processCall(final) {
				break
			}
			continue
		}
		idx, kind, length, hold := s.findMarker(final)
		if idx < 0 {
			end := len(s.buf)
			if hold >= 0 {
				end = hold
			}
			if final {
				// A dangling marker prefix at end of stream is protocol
				// debris: release the prose before it, drop the rest.
				if d := qwenWebDanglingMarkerAt(s.buf[:end]); d >= 0 {
					end = d
					out.WriteString(s.consumeProse(s.buf[:end]))
					s.buf = ""
					break
				}
			}
			out.WriteString(s.consumeProse(s.buf[:end]))
			s.buf = s.buf[end:]
			break
		}
		out.WriteString(s.consumeProse(s.buf[:idx]))
		s.buf = s.buf[idx:]
		if s.inFence {
			// Documentation, not protocol: the marker is literal text.
			out.WriteString(s.consumeProse(s.buf[:length]))
			s.buf = s.buf[length:]
			continue
		}
		switch kind {
		case qwenMarkerOpen:
			s.buf = s.buf[length:]
			s.inCall, s.seekStage, s.tailStage, s.fenceArmed = true, true, false, false
		case qwenMarkerResultOpen:
			if !s.swallowResultBlock(final) {
				return out.String()
			}
		default: // orphan closers: protocol debris
			s.buf = s.buf[length:]
		}
	}
	return out.String()
}

// swallowResultBlock drops a model-faked [TOOL RESULT …] span through
// its [END TOOL RESULT]. It reports false when the end marker has not
// arrived yet and the buffer must be held for more input.
func (s *qwenWebToolScanner) swallowResultBlock(final bool) bool {
	// s.buf starts at the result opener.
	_, openLen, _ := qwenWebClassifyMarker(s.buf, final)
	rest := s.buf[openLen:]
	if end := strings.Index(rest, "[END TOOL RESULT]"); end >= 0 {
		s.buf = rest[end+len("[END TOOL RESULT]"):]
		return true
	}
	if !final && len(rest) < qwenWebResultBlkMax {
		return false // hold the buffer; the end marker may still arrive
	}
	// No end in reach: drop the opener and let the body be re-scanned (a
	// late end marker is swallowed later as an orphan closer).
	s.buf = rest
	return true
}

// findMarker locates the first marker candidate in s.buf. hold is the
// index from which the buffer must be retained (a possible partial
// marker) when no complete marker was found, or -1.
func (s *qwenWebToolScanner) findMarker(final bool) (idx, kind, length, hold int) {
	for i := 0; i < len(s.buf); i++ {
		c := s.buf[i]
		if c != '[' && c != '<' {
			continue
		}
		k, l, more := qwenWebClassifyMarker(s.buf[i:], final)
		if k != qwenMarkerNone {
			return i, k, l, -1
		}
		if more {
			return -1, 0, 0, i
		}
	}
	return -1, 0, 0, -1
}

// processCall advances the in-call state. It reports false when more
// input is needed (and the stream has not ended).
func (s *qwenWebToolScanner) processCall(final bool) bool {
	if s.seekStage {
		return s.seekPayload(final)
	}
	if s.tailStage {
		return s.consumeCallTail(final)
	}
	end, balanced := qwenWebBalancedObjectEnd(s.buf)
	if !balanced {
		if len(s.buf) > qwenWebToolSpanMax || final {
			// Truncated or runaway span: dropped whole, fail-closed.
			s.dropped++
			s.buf, s.inCall = "", false
			return true
		}
		return false
	}
	payload := s.buf[:end]
	if !qwenWebPayloadHasName(payload) {
		// Not a call attempt: release the object as prose by handing the
		// buffer back to the prose state untouched (the trigger is
		// already dropped; a trailing closer becomes an orphan and is
		// swallowed there).
		s.inCall = false
		return true
	}
	s.buf = s.buf[end:]
	if call, ok := qwenWebValidatePayload(payload, s.allowed); ok {
		s.calls = append(s.calls, call)
	} else {
		s.dropped++
	}
	s.tailStage = true
	return s.consumeCallTail(final)
}

// qwenWebPayloadHasName reports whether the payload object carries a
// top-level "name" key. Unparseable payloads count as call attempts
// (validation drops them); only a clean object without the key is
// released as prose.
func qwenWebPayloadHasName(text string) bool {
	var probe struct {
		Name *string `json:"name"`
	}
	if err := stdjson.Unmarshal([]byte(text), &probe); err != nil {
		return true
	}
	return probe.Name != nil
}

// seekPayload finds the payload's opening brace after the open marker.
func (s *qwenWebToolScanner) seekPayload(final bool) bool {
	for {
		s.buf = strings.TrimLeft(s.buf, " \t\r\n")
		if strings.HasPrefix(s.buf, "```") {
			nl := strings.IndexByte(s.buf, '\n')
			if nl < 0 {
				if !final {
					return false
				}
				s.dropped++
				s.buf, s.inCall = "", false
				return true
			}
			s.buf = s.buf[nl+1:]
			s.fenceArmed = true
			continue
		}
		break
	}
	if len(s.buf) == 0 {
		if final {
			s.dropped++
			s.inCall = false
			return true
		}
		return false
	}
	if s.buf[0] == '{' {
		s.seekStage = false
		return true
	}
	// The brace may sit a little behind decoration text (the window the
	// wrapper corpus measured); anything up to '{' inside the window is
	// decoration and drops with the trigger.
	win := qwenWebPayloadWin
	if len(s.buf) < win {
		if !final {
			return false // the brace may still arrive inside the window
		}
		win = len(s.buf)
	}
	if j := strings.IndexByte(s.buf[:win], '{'); j >= 0 {
		s.buf = s.buf[j:]
		s.seekStage = false
		return true
	}
	// No payload in the window: the trigger was quoted prose, not a call.
	// Release the buffered text as prose (the marker itself is already
	// dropped) and return to the prose state.
	s.inCall = false
	return true
}

// consumeCallTail eats what may follow a balanced payload: bounded
// closing debris, the closing marker, and a fence closer if armed. It
// reports false when a decision needs more input.
func (s *qwenWebToolScanner) consumeCallTail(final bool) bool {
	i := 0
	for i < len(s.buf) && isQwenBlank(s.buf[i]) {
		i++
	}
	// Bounded debris (a stray extra '}' or ']'), eaten only when a closer
	// or the end of input follows it — otherwise it is prose.
	j := i
	debris := 0
	for j < len(s.buf) && debris < qwenWebDebrisMax && (s.buf[j] == '}' || s.buf[j] == ']') {
		j++
		debris++
	}
	k := j
	for k < len(s.buf) && isQwenBlank(s.buf[k]) {
		k++
	}
	rest := s.buf[k:]
	if rest == "" {
		if !final {
			s.buf = s.buf[i:] // hold the possible debris until decided
			return false
		}
		s.buf = ""
		s.inCall, s.tailStage = false, false
		return true
	}
	// A fence the payload arrived in may close before the closer marker.
	if s.fenceArmed && strings.HasPrefix(rest, "```") {
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			trimmed := strings.TrimLeft(rest[nl+1:], " \t\r\n")
			kind, length, more := qwenWebClassifyMarker(trimmed, final)
			if kind == qwenMarkerClose {
				s.buf = trimmed[length:]
				s.inCall, s.tailStage = false, false
				return true
			}
			if more && !final {
				// The closer may be complete but for one disambiguating
				// byte; releasing the fence line now would flip the
				// prose fence machine and strand the closer as text.
				s.buf = s.buf[i:]
				return false
			}
		} else if !final {
			s.buf = s.buf[i:]
			return false
		}
	}
	if kind, length, more := qwenWebClassifyMarker(rest, final); kind == qwenMarkerClose {
		s.buf = rest[length:]
		s.inCall, s.tailStage = false, false
		s.swallowFenceClose()
		return true
	} else if more && !final {
		s.buf = s.buf[i:]
		return false
	}
	// Not a closer: the debris was prose after all. Keep everything from
	// the first non-blank onward as prose input.
	s.buf = s.buf[i:]
	s.inCall, s.tailStage = false, false
	return true
}

// swallowFenceClose consumes a closing ``` line when the payload arrived
// fenced. Best-effort and non-blocking: absence is fine.
func (s *qwenWebToolScanner) swallowFenceClose() {
	trimmed := strings.TrimLeft(s.buf, " \t\r\n")
	if !strings.HasPrefix(trimmed, "```") {
		return
	}
	if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
		s.buf = trimmed[nl+1:]
	} else {
		s.buf = trimmed
	}
}

func isQwenBlank(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// consumeProse releases text as client-visible prose, running it through
// the code-fence machine so a marker inside a fenced example stays text.
func (s *qwenWebToolScanner) consumeProse(text string) string {
	for i := 0; i < len(text); i++ {
		c := text[i]
		if s.btArmed {
			if c == '`' {
				s.btRun++
				continue
			}
			if s.btRun >= 3 {
				s.inFence = !s.inFence
			}
			s.btArmed, s.btRun = false, 0
		}
		switch {
		case c == '\n':
			s.linePos = 0
		case s.linePos == 0 && c == '`':
			s.btArmed, s.btRun = true, 1
		case s.linePos == 0 && (c == ' ' || c == '\t' || c == '\r'):
			// still blank on this line
		default:
			s.linePos = 1
		}
	}
	return text
}

// qwenWebClassifyMarker classifies a marker at the start of s (s[0] is
// '[' or '<'). more=true means s is a live prefix of a marker and the
// decision needs more input (never returned when final is true).
func qwenWebClassifyMarker(s string, final bool) (kind, length int, more bool) {
	if len(s) > qwenWebMarkerMax {
		s = s[:qwenWebMarkerMax]
	}
	if s[0] == '[' {
		return qwenWebClassifyBracket(s, final)
	}
	return qwenWebClassifyAngle(s, final)
}

// markerWord matches a keyword at s[i:] case-insensitively, allowing the
// run to end mid-word at end of input (reported as more).
type qwenMarkerCursor struct {
	s     string
	i     int
	final bool
}

func (c *qwenMarkerCursor) blanks(max int) bool {
	n := 0
	for c.i < len(c.s) && (c.s[c.i] == ' ' || c.s[c.i] == '\t') && n < max {
		c.i++
		n++
	}
	return c.i < len(c.s) || c.final
}

func (c *qwenMarkerCursor) word(w string) (ok, more bool) {
	for j := 0; j < len(w); j++ {
		if c.i+j >= len(c.s) {
			return false, !c.final
		}
		if upperByte(c.s[c.i+j]) != w[j] {
			return false, false
		}
	}
	c.i += len(w)
	return true, false
}

func (c *qwenMarkerCursor) sep() (ok, more bool) {
	n := 0
	for c.i < len(c.s) && n < 2 && (c.s[c.i] == ' ' || c.s[c.i] == '\t' || c.s[c.i] == '_' || c.s[c.i] == '-') {
		c.i++
		n++
	}
	if n > 0 {
		return true, false
	}
	if c.i >= len(c.s) {
		return false, !c.final
	}
	return false, false
}

func upperByte(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}

func qwenWebClassifyBracket(s string, final bool) (int, int, bool) {
	c := &qwenMarkerCursor{s: s, i: 1, final: final}
	if !c.blanks(4) {
		return qwenMarkerNone, 0, !final
	}
	kind := qwenMarkerOpen
	if c.i < len(s) && s[c.i] == '/' {
		kind = qwenMarkerClose
		c.i++
		if !c.blanks(4) {
			return qwenMarkerNone, 0, !final
		}
	} else if ok, more := c.word("END"); ok {
		kind = qwenMarkerClose
		if ok2, more2 := c.sep(); !ok2 {
			return qwenMarkerNone, 0, more2
		}
	} else if more {
		return qwenMarkerNone, 0, true
	}
	if ok, more := c.word("TOOL"); !ok {
		return qwenMarkerNone, 0, more
	}
	if ok, more := c.sep(); !ok {
		return qwenMarkerNone, 0, more
	}
	if ok, more := c.word("CALL"); ok {
		// canonical call marker; decoration up to ']' follows
	} else if ok2, more2 := c.word("RESULT"); ok2 {
		if kind == qwenMarkerClose {
			kind = qwenMarkerResultClose
		} else if kind == qwenMarkerOpen {
			kind = qwenMarkerResultOpen
		} else {
			return qwenMarkerNone, 0, false
		}
	} else {
		return qwenMarkerNone, 0, more || more2
	}
	// Decoration up to the closing ']': single line, bounded, and a "]("
	// tail means this was a Markdown link, not a marker.
	start := c.i
	for c.i < len(s) {
		ch := s[c.i]
		if ch == ']' {
			if c.i+1 < len(s) && s[c.i+1] == '(' {
				return qwenMarkerNone, 0, false
			}
			if c.i+1 >= len(s) && !final {
				return qwenMarkerNone, 0, true
			}
			return kind, c.i + 1, false
		}
		if ch == '[' || ch == '\n' || c.i-start > 40 {
			return qwenMarkerNone, 0, false
		}
		c.i++
	}
	return qwenMarkerNone, 0, !final
}

func qwenWebClassifyAngle(s string, final bool) (int, int, bool) {
	c := &qwenMarkerCursor{s: s, i: 1, final: final}
	if !c.blanks(4) {
		return qwenMarkerNone, 0, !final
	}
	kind := qwenMarkerOpen
	if c.i < len(s) && s[c.i] == '/' {
		kind = qwenMarkerClose
		c.i++
		if !c.blanks(4) {
			return qwenMarkerNone, 0, !final
		}
	}
	if ok, more := c.word("TOOL"); !ok {
		return qwenMarkerNone, 0, more
	}
	if c.i < len(s) && s[c.i] == '_' {
		c.i++
	} else if c.i >= len(s) {
		return qwenMarkerNone, 0, !final
	} else {
		return qwenMarkerNone, 0, false
	}
	if ok, more := c.word("CALL"); !ok {
		return qwenMarkerNone, 0, more
	}
	if c.i < len(s) && (s[c.i] == 'S' || s[c.i] == 's') {
		c.i++
	}
	start := c.i
	for c.i < len(s) {
		ch := s[c.i]
		if ch == '>' {
			return kind, c.i + 1, false
		}
		if ch == '<' || ch == '\n' || c.i-start > 56 {
			return qwenMarkerNone, 0, false
		}
		c.i++
	}
	return qwenMarkerNone, 0, !final
}

// qwenWebDanglingMarkerAt finds the first position in s where a marker
// prefix runs to the end of input uncompleted — protocol debris at end
// of stream (a half-written "[TOOL CALL #3" or "<tool_c"). Only called
// at finish, on a buffer the classifier already found no complete
// marker in. Returns -1 when there is none.
func qwenWebDanglingMarkerAt(s string) int {
	canonical := []string{
		"[TOOL CALL", "[TOOL RESULT", "[END TOOL CALL", "[END TOOL RESULT",
		"[/TOOL CALL", "<TOOL_CALL", "<TOOL_CALLS", "</TOOL_CALL", "</TOOL_CALLS",
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '[' && s[i] != '<' {
			continue
		}
		tail := s[i:]
		if strings.Contains(tail, "\n") {
			continue
		}
		upper := strings.ToUpper(tail)
		for _, canon := range canonical {
			if len(upper) >= 2 && strings.HasPrefix(canon, upper) {
				return i
			}
			if strings.HasPrefix(upper, canon) {
				// Keyword complete, decoration/closer still open.
				if canon[0] == '[' && !strings.ContainsAny(tail[1:], "][") && len(tail) <= 41 {
					return i
				}
				if canon[0] == '<' && !strings.ContainsAny(tail[1:], "><") && len(tail) <= 64 {
					return i
				}
			}
		}
	}
	return -1
}

// qwenWebBalancedObjectEnd returns the end index (exclusive) of the JSON
// object starting at s[0] (which must be '{'), string-aware. balanced is
// false when the object has not closed yet.
func qwenWebBalancedObjectEnd(s string) (end int, balanced bool) {
	depth := 0
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// qwenWebValidatePayload parses one payload object into a tool call. The
// name must be declared for the request; arguments must canonicalise to
// a JSON object (a string value is unwrapped one level first). A strict
// parse failure gets one deterministic repair pass (raw control
// characters inside string literals — the one model fault with no
// semantic ambiguity), never a lenient parser.
func qwenWebValidatePayload(text string, allowed map[string]bool) (qwenWebToolCall, bool) {
	call, ok := qwenWebParsePayload(text)
	if !ok {
		if repaired := qwenWebRepairControlChars(text); repaired != "" {
			call, ok = qwenWebParsePayload(repaired)
		}
	}
	if !ok || !allowed[call.Name] {
		return qwenWebToolCall{}, false
	}
	return call, true
}

func qwenWebParsePayload(text string) (qwenWebToolCall, bool) {
	dec := stdjson.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var payload struct {
		Name      string             `json:"name"`
		Arguments stdjson.RawMessage `json:"arguments"`
	}
	if err := dec.Decode(&payload); err != nil || payload.Name == "" {
		return qwenWebToolCall{}, false
	}
	args := "{}"
	if len(payload.Arguments) > 0 && string(payload.Arguments) != "null" {
		canonical, ok := qwenWebCanonicalArguments(payload.Arguments)
		if !ok {
			return qwenWebToolCall{}, false
		}
		args = canonical
	}
	return qwenWebToolCall{Name: payload.Name, Arguments: args}, true
}

// qwenWebCanonicalArguments canonicalises an arguments value to a
// compact JSON object string. A JSON string value is unwrapped: its
// content must itself be a JSON object.
func qwenWebCanonicalArguments(raw stdjson.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var inner string
		if err := stdjson.Unmarshal(trimmed, &inner); err != nil {
			return "", false
		}
		return qwenWebCanonicalJSON(inner)
	}
	return qwenWebCanonicalJSON(string(trimmed))
}

// qwenWebCanonicalJSON parses s and re-marshals it compactly; the value
// must be a JSON object.
func qwenWebCanonicalJSON(s string) (string, bool) {
	dec := stdjson.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	if _, isObj := v.(map[string]any); !isObj {
		return "", false
	}
	out, err := stdjson.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// qwenWebRepairControlChars escapes raw C0 control characters inside
// JSON string literals (models paste multi-line text into argument
// strings verbatim). Returns "" when nothing was repaired. Only ever
// applied after a strict parse has already failed, so valid JSON is
// never touched.
func qwenWebRepairControlChars(text string) string {
	var b strings.Builder
	b.Grow(len(text) + 8)
	inStr, esc, repaired := false, false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if !inStr {
			if c == '"' {
				inStr = true
			}
			b.WriteByte(c)
			continue
		}
		switch {
		case esc:
			esc = false
			b.WriteByte(c)
		case c == '\\':
			esc = true
			b.WriteByte(c)
		case c == '"':
			inStr = false
			b.WriteByte(c)
		case c < 0x20:
			repaired = true
			switch c {
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				b.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			}
		default:
			b.WriteByte(c)
		}
	}
	if !repaired {
		return ""
	}
	return b.String()
}

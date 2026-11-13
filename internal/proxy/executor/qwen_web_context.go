package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Context externalization for qw-web (2026-11-03). The lane flattens a
// whole OpenAI request into ONE user message, and chat.qwen.ai's risk
// control refuses request bodies past roughly 128 KiB (RGV587) — a size
// a Hermes agent session crosses with its system prompt, tool catalogue
// and history combined (measured live: a ±177 KB body refused on every
// retry while a 21-token turn beside it succeeded). The site's own
// escape is a document attachment: the full context is uploaded once,
// parsed upstream, and referenced from the message's files array, while
// a budgeted live prompt (head + recent turns + current turn) stays
// inline. The design follows the two maintained wrappers' measured
// behaviour — Rfym21/Qwen2API (payload-triggered externalization, 90 KiB
// trigger, 48 KiB live budget, history-prefix reuse, compaction only as
// a no-tools fallback) and YuJunZhiXue/qwen2API (v2 STS endpoint) —
// adapted to this executor's block-structured transcript.

const (
	// Trigger on the estimated serialized completion body, the wrappers'
	// unit: past this, externalize or compact before anything is sent.
	qwenWebContextTriggerBytes = 90 * 1024
	// What stays inline next to an attachment.
	qwenWebLiveBudgetBytes = 48 * 1024
	// No-tools compaction fallback budget (the serialized body must end
	// under the trigger; the fallback text budget sits below it).
	qwenWebFallbackBudgetBytes = 84 * 1024
	// Margin when judging whether a baked history prefix leaves the
	// live body under the trigger (descriptor size varies a little).
	qwenWebBakeFitMargin = 2048
	// Prefix cache bounds: parsed attachments die upstream after a
	// while and the model then silently loses the file, so entries are
	// short-lived and few (the small-box rule: every cache is bounded).
	qwenWebPrefixTTL       = 30 * time.Minute
	qwenWebPrefixMaxPerKey = 4
)

// qwenWebBlock is one segment of the flattened transcript, tagged so the
// context planner can reason about it without re-parsing text: head
// blocks are system/developer text and the tool protocol, turn blocks
// are labelled conversation, a bare block is a lone user message.
type qwenWebBlock struct {
	head bool
	text string
}

type qwenWebContextParts struct {
	blocks []qwenWebBlock
}

// Full renders the complete flattened content — byte-identical to the
// pre-externalization rendering, so an under-threshold request is
// untouched by this layer.
func (p *qwenWebContextParts) Full() string {
	texts := make([]string, 0, len(p.blocks))
	for _, b := range p.blocks {
		texts = append(texts, b.text)
	}
	return strings.Join(texts, "\n\n")
}

func (p *qwenWebContextParts) headText() string {
	var head []string
	for _, b := range p.blocks {
		if b.head {
			head = append(head, b.text)
		}
	}
	return strings.Join(head, "\n\n")
}

// turns returns the conversation blocks in order; the last one is the
// current turn.
func (p *qwenWebContextParts) turns() []string {
	var turns []string
	for _, b := range p.blocks {
		if !b.head {
			turns = append(turns, b.text)
		}
	}
	return turns
}

// qwenWebBuildContext renders the request into blocks. It is the single
// renderer behind both the plain and tool flatten paths.
func qwenWebBuildContext(in []qwenWebInputMsg, toolCtx *qwenWebToolContext) *qwenWebContextParts {
	if toolCtx != nil {
		return qwenWebBuildToolContext(in, toolCtx)
	}
	parts := &qwenWebContextParts{}
	type turn struct{ role, text string }
	var turns []turn
	for _, m := range in {
		text := qwenWebTextContent(m.Content)
		if text == "" {
			continue
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		turns = append(turns, turn{role, text})
	}
	if len(turns) == 1 && turns[0].role == "user" {
		parts.blocks = append(parts.blocks, qwenWebBlock{text: turns[0].text})
		return parts
	}
	for _, t := range turns {
		switch t.role {
		case "system", "developer":
			parts.blocks = append(parts.blocks, qwenWebBlock{head: true, text: t.text})
		case "assistant":
			parts.blocks = append(parts.blocks, qwenWebBlock{text: "Assistant: " + t.text})
		default:
			parts.blocks = append(parts.blocks, qwenWebBlock{text: "User: " + t.text})
		}
	}
	return parts
}

// qwenWebBuildToolContext is the tool-mode renderer: system text and the
// protocol form the head; calls and results fold into the bracket-marker
// blocks (numbered across the whole history, results echoing their
// call's arguments — the ds-web binding rule).
func qwenWebBuildToolContext(in []qwenWebInputMsg, toolCtx *qwenWebToolContext) *qwenWebContextParts {
	parts := &qwenWebContextParts{}
	type callRef struct {
		ordinal int
		name    string
		args    string
	}
	byID := map[string]callRef{}
	ordinal := 0
	loneUser := ""
	loneIdx := -1
	for _, m := range in {
		text := qwenWebTextContent(m.Content)
		role := m.Role
		if role == "" {
			role = "user"
		}
		switch role {
		case "system", "developer":
			if text != "" {
				parts.blocks = append(parts.blocks, qwenWebBlock{head: true, text: text})
			}
		case "assistant":
			calls := m.allCalls()
			if len(calls) == 0 {
				if text != "" {
					parts.blocks = append(parts.blocks, qwenWebBlock{text: "Assistant: " + text})
				}
				continue
			}
			blocks := make([]string, 0, len(calls))
			for _, c := range calls {
				id, name, args := c.resolved()
				if name == "" {
					continue
				}
				ordinal++
				if id != "" {
					byID[id] = callRef{ordinal, name, args}
				}
				blocks = append(blocks, fmt.Sprintf("[TOOL CALL #%d]\n{\"name\": %s, \"arguments\": %s}\n[END TOOL CALL]",
					ordinal, qwenWebJSONString(name), args))
			}
			if len(blocks) == 0 {
				if text != "" {
					parts.blocks = append(parts.blocks, qwenWebBlock{text: "Assistant: " + text})
				}
				continue
			}
			label := "Assistant:"
			if text != "" {
				label = "Assistant: " + text
			}
			parts.blocks = append(parts.blocks, qwenWebBlock{text: label + "\n" + strings.Join(blocks, "\n")})
		case "tool", "function":
			body := text
			if body == "" {
				body = "(empty)"
			}
			ref, known := byID[m.ToolCallID]
			var header string
			if known {
				header = fmt.Sprintf("[TOOL RESULT #%d: %s]", ref.ordinal, qwenWebMarkerName(ref.name))
			} else {
				name := m.Name
				if name == "" {
					name = "tool"
				}
				header = fmt.Sprintf("[TOOL RESULT: %s]", qwenWebMarkerName(name))
			}
			block := header + "\n"
			if known {
				block += "Arguments: " + ref.args + "\n"
			}
			block += body + "\n[END TOOL RESULT]"
			parts.blocks = append(parts.blocks, qwenWebBlock{text: "User:\n" + block})
		default:
			if text != "" {
				parts.blocks = append(parts.blocks, qwenWebBlock{text: "User: " + text})
				loneUser = text
				loneIdx = len(parts.blocks) - 1
			}
		}
	}
	parts.blocks = append(parts.blocks, qwenWebBlock{head: true, text: toolCtx.protocol})
	// The protocol head must lead the transcript, not trail it: rebuild
	// with head blocks first (their relative order is preserved).
	ordered := make([]qwenWebBlock, 0, len(parts.blocks))
	for _, b := range parts.blocks {
		if b.head {
			ordered = append(ordered, b)
		}
	}
	for _, b := range parts.blocks {
		if !b.head {
			ordered = append(ordered, b)
		}
	}
	parts.blocks = ordered
	// A lone user message goes out bare after the head, as before.
	if loneIdx >= 0 && len(parts.turns()) == 1 {
		for i, b := range parts.blocks {
			if !b.head {
				parts.blocks[i].text = loneUser
			}
		}
	}
	return parts
}

// qwenWebJSONString quotes a string as JSON without pulling strconv
// semantics into this file's call sites.
func qwenWebJSONString(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(raw)
}

// --- prefix cache -----------------------------------------------------

type qwenWebPrefixEntry struct {
	text       string // the externalized history text (turns joined)
	turnCount  int    // how many leading turns that text covers
	descriptor map[string]any
	expires    time.Time
}

var qwenWebPrefixCache = struct {
	sync.Mutex
	byKey map[string][]qwenWebPrefixEntry
}{byKey: map[string][]qwenWebPrefixEntry{}}

func qwenWebPrefixKey(cred qwenWebCredential) string {
	sum := sha256.Sum256([]byte(cred.Token))
	return hex.EncodeToString(sum[:])[:16]
}

// qwenWebPrefixLookup finds a cached attachment whose text is a prefix
// of the history being sent (the whole history, or its leading turns).
func qwenWebPrefixLookup(key, history string, now time.Time) (qwenWebPrefixEntry, bool) {
	qwenWebPrefixCache.Lock()
	defer qwenWebPrefixCache.Unlock()
	entries := qwenWebPrefixCache.byKey[key]
	kept := entries[:0]
	var found qwenWebPrefixEntry
	ok := false
	for _, e := range entries {
		if now.After(e.expires) {
			continue
		}
		kept = append(kept, e)
		if history == e.text || strings.HasPrefix(history, e.text+"\n\n") {
			if !ok || len(e.text) > len(found.text) {
				found, ok = e, true
			}
		}
	}
	qwenWebPrefixCache.byKey[key] = kept
	return found, ok
}

func qwenWebPrefixStore(key string, entry qwenWebPrefixEntry) {
	qwenWebPrefixCache.Lock()
	defer qwenWebPrefixCache.Unlock()
	entries := qwenWebPrefixCache.byKey[key]
	entries = append(entries, entry)
	if len(entries) > qwenWebPrefixMaxPerKey {
		entries = entries[len(entries)-qwenWebPrefixMaxPerKey:]
	}
	qwenWebPrefixCache.byKey[key] = entries
}

// --- budgeting --------------------------------------------------------

const qwenWebCompactSeparator = "\n...[inline context compacted; the complete copy is in the attachment]...\n"

// qwenWebHeadTail truncates a text to at most max bytes, keeping the
// head and tail around a marker (UTF-8 safe: it cuts on rune bounds).
func qwenWebHeadTail(s string, max int, headRatio float64) string {
	if len(s) <= max || max <= 0 {
		return s
	}
	sep := qwenWebCompactSeparator
	if max <= len(sep)+64 {
		return qwenWebTruncateRunes(s, max)
	}
	budget := max - len(sep)
	headLen := int(float64(budget) * headRatio)
	head := qwenWebTruncateRunes(s, headLen)
	tail := qwenWebTruncateRunesTail(s, budget-headLen)
	return head + sep + tail
}

func qwenWebTruncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

func qwenWebTruncateRunesTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return s[cut:]
}

// qwenWebBudgetedPrompt renders a reduced live prompt from the parts:
// the notice first, then the head, then as many recent turns as fit
// (whole blocks, newest first), then the current turn — never past the
// byte budget. Used both beside an attachment (48 KiB) and as the
// no-tools compaction fallback (84 KiB, different notice).
func qwenWebBudgetedPrompt(parts *qwenWebContextParts, budget int, notice string) string {
	turns := parts.turns()
	if len(turns) == 0 {
		return qwenWebHeadTail(parts.Full(), budget, 0.55)
	}
	current := turns[len(turns)-1]
	history := turns[:len(turns)-1]
	head := parts.headText()

	used := len(notice)
	cur := current
	if len(cur) > budget/2 {
		cur = qwenWebHeadTail(cur, budget/2, 0.45)
	}
	used += len(cur)
	headBudget := budget - used - 2*len("\n\n")
	if headBudget < 0 {
		headBudget = 0
	}
	if len(head) > headBudget {
		head = qwenWebHeadTail(head, max(headBudget, 256), 0.55)
	}
	used += len(head)

	remaining := budget - used - len("\n\n")
	var recent []string
	for i := len(history) - 1; i >= 0; i-- {
		block := history[i]
		cost := len(block) + len("\n\n")
		if cost > remaining {
			if len(recent) == 0 && remaining > 2048 {
				recent = append([]string{qwenWebHeadTail(block, remaining-len("\n\n"), 0.4)}, recent...)
			}
			break
		}
		recent = append([]string{block}, recent...)
		remaining -= cost
	}

	sections := []string{}
	if notice != "" {
		sections = append(sections, notice)
	}
	if head != "" {
		sections = append(sections, head)
	}
	sections = append(sections, recent...)
	sections = append(sections, cur)
	out := strings.Join(sections, "\n\n")
	if len(out) > budget {
		out = qwenWebHeadTail(out, budget, 0.55)
	}
	return out
}

func qwenWebAttachmentNotice(filename string) string {
	return "# Agent context attachment\n" +
		"The complete system instructions, tool definitions, conversation history and current task are attached as " + filename + ". " +
		"Read that attachment as authoritative context before acting. The essential state and the most recent progress are also retained inline below; " +
		"continue from the latest state and do not restart the task. When a tool is needed, emit the real [TOOL CALL] block immediately."
}

func qwenWebCompactionNotice() string {
	return "# Context note\n" +
		"Older context was compacted to stay below the Qwen Web request limit. The system instructions, the most recent progress and the current task below remain authoritative; continue the latest task from that state."
}

// --- the planner ------------------------------------------------------

// qwenWebPrepareContext applies the externalization plan to the built
// wire messages and returns the messages to actually send. Under the
// trigger it returns them untouched. A tool-carrying request whose
// context cannot be externalized fails honestly (a silently truncated
// agent history replays finished work — the wrappers measured that
// failure mode); a plain chat falls back to compaction instead.
func qwenWebPrepareContext(ctx context.Context, client *http.Client, cred qwenWebCredential, parts *qwenWebContextParts, toolCtx *qwenWebToolContext, build func(content string, files []any) []qwenWebWireMessage) ([]qwenWebWireMessage, error) {
	full := parts.Full()
	msgs := build(full, nil)
	if qwenWebSerializedSize(msgs) <= qwenWebContextTriggerBytes {
		return msgs, nil
	}
	if !cred.LoggedIn {
		if toolCtx != nil {
			return nil, qwenWebFailure(http.StatusServiceUnavailable,
				"qw-web: request context exceeds the Qwen Web body limit and guest mode cannot externalize it — sign in or shorten the session")
		}
		return build(qwenWebBudgetedPrompt(parts, qwenWebFallbackBudgetBytes, qwenWebCompactionNotice()), nil), nil
	}

	prepared, err := qwenWebExternalize(ctx, client, cred, parts, build)
	if err == nil {
		return prepared, nil
	}
	if toolCtx != nil {
		return nil, qwenWebFailure(http.StatusServiceUnavailable,
			"qw-web: request context exceeds the Qwen Web body limit and externalizing it failed ("+err.Error()+") — retry shortly")
	}
	return build(qwenWebBudgetedPrompt(parts, qwenWebFallbackBudgetBytes, qwenWebCompactionNotice()), nil), nil
}

// qwenWebExternalize tries, in order: reuse a cached history attachment,
// bake the history into a new attachment, or attach the whole context.
func qwenWebExternalize(ctx context.Context, client *http.Client, cred qwenWebCredential, parts *qwenWebContextParts, build func(content string, files []any) []qwenWebWireMessage) ([]qwenWebWireMessage, error) {
	turns := parts.turns()
	if len(turns) >= 2 {
		history := strings.Join(turns[:len(turns)-1], "\n\n")
		if msgs, ok := qwenWebReusePrefix(cred, parts, history, build); ok {
			return msgs, nil
		}
		if msgs, err := qwenWebBakePrefix(ctx, client, cred, parts, history, build); err == nil {
			return msgs, nil
		}
		// A failed bake falls through to the whole-context attachment:
		// when the head or current turn dominates, that is the right
		// shape anyway, and when the bake failed transiently the whole
		// context still has its own chance.
	}
	filename := fmt.Sprintf("QW9R_AGENT_CONTEXT_%d.txt", time.Now().Unix())
	desc, err := qwenWebUploadContext(ctx, client, cred, filename, parts.Full())
	if err != nil {
		return nil, err
	}
	live := qwenWebBudgetedPrompt(parts, qwenWebLiveBudgetBytes, qwenWebAttachmentNotice(filename))
	return build(live, []any{desc}), nil
}

// qwenWebReusePrefix serves the turn from a cached history attachment:
// head + a continuation line + only the turns the attachment does not
// already cover + the current turn stay inline.
func qwenWebReusePrefix(cred qwenWebCredential, parts *qwenWebContextParts, history string, build func(content string, files []any) []qwenWebWireMessage) ([]qwenWebWireMessage, bool) {
	entry, ok := qwenWebPrefixLookup(qwenWebPrefixKey(cred), history, time.Now())
	if !ok {
		return nil, false
	}
	turns := parts.turns()
	tail := turns[entry.turnCount : len(turns)-1]
	name, _ := entry.descriptor["name"].(string)
	line := fmt.Sprintf("[earlier history: the first %d messages are in the attachment %s; the conversation below continues from message %d]",
		entry.turnCount, name, entry.turnCount+1)
	sections := []string{qwenWebAttachmentNotice(name)}
	if head := parts.headText(); head != "" {
		sections = append(sections, head)
	}
	sections = append(sections, line)
	sections = append(sections, tail...)
	sections = append(sections, turns[len(turns)-1])
	msgs := build(strings.Join(sections, "\n\n"), []any{entry.descriptor})
	if qwenWebSerializedSize(msgs) > qwenWebContextTriggerBytes {
		return nil, false
	}
	return msgs, true
}

// qwenWebBakePrefix uploads the history (all turns but the current one)
// as the attachment and keeps head + current inline, caching the result
// so the session's next turns reuse it instead of paying another parse.
func qwenWebBakePrefix(ctx context.Context, client *http.Client, cred qwenWebCredential, parts *qwenWebContextParts, history string, build func(content string, files []any) []qwenWebWireMessage) ([]qwenWebWireMessage, error) {
	filename := fmt.Sprintf("QW9R_AGENT_HISTORY_%d.txt", time.Now().Unix())
	line := fmt.Sprintf("[earlier history: the first %d messages are in the attachment %s]", len(parts.turns())-1, filename)
	sections := []string{qwenWebAttachmentNotice(filename)}
	if head := parts.headText(); head != "" {
		sections = append(sections, head)
	}
	sections = append(sections, line)
	turns := parts.turns()
	sections = append(sections, turns[len(turns)-1])
	probe := build(strings.Join(sections, "\n\n"), []any{qwenWebPlaceholderDescriptor(filename, len(history))})
	if qwenWebSerializedSize(probe)+qwenWebBakeFitMargin > qwenWebContextTriggerBytes {
		return nil, fmt.Errorf("qw-web: head and current turn alone exceed the body limit")
	}
	desc, err := qwenWebUploadContext(ctx, client, cred, filename, history)
	if err != nil {
		return nil, err
	}
	qwenWebPrefixStore(qwenWebPrefixKey(cred), qwenWebPrefixEntry{
		text: history, turnCount: len(turns) - 1, descriptor: desc,
		expires: time.Now().Add(qwenWebPrefixTTL),
	})
	return build(strings.Join(sections, "\n\n"), []any{desc}), nil
}

// qwenWebPlaceholderDescriptor stands in for the real descriptor when
// measuring whether the baked shape fits (same size class).
func qwenWebPlaceholderDescriptor(filename string, size int) map[string]any {
	return map[string]any{
		"type": "file",
		"file": map[string]any{
			"id": "00000000-0000-4000-8000-000000000000", "filename": filename, "name": filename,
			"size": size, "type": "text/plain",
			"meta": map[string]any{"name": filename, "size": size, "content_type": "text/plain",
				"parse_meta": map[string]any{"parse_status": "success"}},
			"hash": nil, "data": map[string]any{},
			"created_at": 0, "update_at": 0,
		},
		"id": "00000000-0000-4000-8000-000000000000",
		"url": "https://qwen-webui-prod.oss-accelerate.aliyuncs.com/placeholder/" + filename,
		"name": filename, "collection_name": "", "status": "uploaded", "progress": 100,
		"greenNet": "success", "file_type": "text/plain", "showType": "file",
		"file_class": "document", "context": "full",
	}
}

// qwenWebSerializedSize estimates the completion body's wire size from
// the messages array (the body adds only fixed fields around it).
func qwenWebSerializedSize(msgs []qwenWebWireMessage) int {
	raw, err := json.Marshal(msgs)
	if err != nil {
		return 0
	}
	return len(raw) + 512
}

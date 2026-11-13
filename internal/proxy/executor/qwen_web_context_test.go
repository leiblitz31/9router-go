package executor

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// qwenWebRewriteTransport points every request at the test server, so
// the file endpoints (pinned to the production host by a const) can be
// exercised offline.
type qwenWebRewriteTransport struct{ target string }

func (t qwenWebRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(t.target, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}

func qwenWebTestClient(server *httptest.Server) *http.Client {
	return &http.Client{Transport: qwenWebRewriteTransport{target: server.URL}}
}

func qwenWebMsg(role, text string) qwenWebInputMsg {
	raw, _ := json.Marshal(text)
	return qwenWebInputMsg{Role: role, Content: jsontext.Value(raw)}
}

func TestQwenWebHeadTailUTF8(t *testing.T) {
	s := strings.Repeat("é", 5000) // 2-byte runes: a byte cut lands mid-rune
	out := qwenWebHeadTail(s, 1024, 0.55)
	if len(out) > 1024 {
		t.Fatalf("head/tail length = %d, want <= 1024", len(out))
	}
	if !strings.Contains(out, "[inline context compacted") {
		t.Errorf("truncation marker missing")
	}
	if strings.Contains(out, "�") {
		t.Errorf("output contains a replacement char — a rune was split")
	}
}

func TestQwenWebBudgetedPromptKeepsHeadAndCurrent(t *testing.T) {
	in := []qwenWebInputMsg{qwenWebMsg("system", "SYSHEAD-RULES")}
	for i := 0; i < 12; i++ {
		in = append(in, qwenWebMsg("user", fmt.Sprintf("turn-%02d ", i)+strings.Repeat("x", 4000)))
		in = append(in, qwenWebMsg("assistant", fmt.Sprintf("reply-%02d ", i)+strings.Repeat("y", 4000)))
	}
	in = append(in, qwenWebMsg("user", "CURRENT-QUESTION"))
	parts := qwenWebBuildContext(in, nil)
	out := qwenWebBudgetedPrompt(parts, 8192, qwenWebCompactionNotice())
	if len(out) > 8192 {
		t.Fatalf("budgeted prompt = %d bytes, want <= 8192", len(out))
	}
	if !strings.Contains(out, "CURRENT-QUESTION") {
		t.Errorf("current turn was lost")
	}
	if !strings.Contains(out, "SYSHEAD-RULES") {
		t.Errorf("system head was lost")
	}
}

func TestQwenWebPrefixCache(t *testing.T) {
	key := "testkey"
	entry := qwenWebPrefixEntry{
		text: "User: one\n\nAssistant: two", turnCount: 2,
		descriptor: map[string]any{"name": "hist.txt"},
		expires:    time.Now().Add(time.Minute),
	}
	qwenWebPrefixStore(key, entry)
	if _, ok := qwenWebPrefixLookup(key, "User: one\n\nAssistant: two", time.Now()); !ok {
		t.Errorf("exact history did not hit the cache")
	}
	if _, ok := qwenWebPrefixLookup(key, "User: one\n\nAssistant: two\n\nUser: three", time.Now()); !ok {
		t.Errorf("extended history did not hit the cached prefix")
	}
	if _, ok := qwenWebPrefixLookup(key, "User: unrelated", time.Now()); ok {
		t.Errorf("unrelated history hit the cache")
	}
	qwenWebPrefixStore(key, qwenWebPrefixEntry{
		text: "User: old", turnCount: 1, descriptor: map[string]any{"name": "old.txt"},
		expires: time.Now().Add(-time.Minute),
	})
	if _, ok := qwenWebPrefixLookup(key, "User: old", time.Now()); ok {
		t.Errorf("expired entry hit the cache")
	}
}

func TestQwenWebPrepareGuestCompactionAndToolRefusal(t *testing.T) {
	buildParts := func() *qwenWebContextParts {
		in := []qwenWebInputMsg{qwenWebMsg("system", "SYSHEAD")}
		for i := 0; i < 16; i++ {
			in = append(in, qwenWebMsg("user", fmt.Sprintf("bulk-%02d ", i)+strings.Repeat("z", 9000)))
		}
		in = append(in, qwenWebMsg("user", "CURRENT-QUESTION"))
		return qwenWebBuildContext(in, nil)
	}
	build := func(content string, files []any) []qwenWebWireMessage {
		return qwenWebWireFromParts("qwen3.7-plus", true, content, files)
	}
	ctx := context.Background()

	msgs, err := qwenWebPrepareContext(ctx, http.DefaultClient, qwenWebCredential{}, buildParts(), nil, build)
	if err != nil {
		t.Fatalf("guest compaction: %v", err)
	}
	if got := qwenWebSerializedSize(msgs); got > qwenWebContextTriggerBytes {
		t.Errorf("compacted body = %d bytes, want <= %d", got, qwenWebContextTriggerBytes)
	}
	if !strings.Contains(msgs[0].Content, "CURRENT-QUESTION") {
		t.Errorf("compacted prompt lost the current turn")
	}

	toolCtx := &qwenWebToolContext{allowed: map[string]bool{"x": true}}
	if _, err := qwenWebPrepareContext(ctx, http.DefaultClient, qwenWebCredential{}, buildParts(), toolCtx, build); err == nil {
		t.Errorf("oversized guest tool request was not refused")
	}
}

// TestQwenWebPrepareExternalizes runs the whole plan against a fake
// Qwen: STS, a signed PUT (the real implementation, aimed at the fake
// endpoint), parse + status — then a second, longer turn that must
// reuse the baked history attachment instead of uploading again.
func TestQwenWebPrepareExternalizes(t *testing.T) {
	var stsCalls, putCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "wu.json"):
			_, _ = io.WriteString(w, `umx.wu('FAKEUMID')`)
		case strings.HasSuffix(r.URL.Path, "/files/getstsToken"):
			stsCalls.Add(1)
			_, _ = fmt.Fprintf(w, `{"success":true,"data":{"access_key_id":"ak","access_key_secret":"sk","security_token":"st","file_id":"fid-1","file_path":"u/f.txt","bucketname":"bkt","region":"oss-ap-southeast-1","endpoint":%q}}`, "http://"+r.Host)
		case r.Method == http.MethodPut:
			putCalls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/files/parse/status"):
			_, _ = io.WriteString(w, `{"success":true,"data":[{"status":"success"}]}`)
		case strings.HasSuffix(r.URL.Path, "/files/parse"):
			_, _ = io.WriteString(w, `{"success":true,"data":{}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := qwenWebTestClient(server)
	cred := qwenWebCredential{Token: "jwt-test", LoggedIn: true}
	build := func(content string, files []any) []qwenWebWireMessage {
		return qwenWebWireFromParts("qwen3.7-plus", true, content, files)
	}
	bulk := func(tag string) string { return tag + " " + strings.Repeat("b", 30000) }
	turns := []qwenWebInputMsg{
		qwenWebMsg("system", "SYSHEAD"),
		qwenWebMsg("user", bulk("t1")),
		qwenWebMsg("assistant", bulk("t2")),
		qwenWebMsg("user", bulk("t3")),
		qwenWebMsg("assistant", bulk("t4")),
		qwenWebMsg("user", "CURRENT-1"),
	}
	ctx := context.Background()
	msgs, err := qwenWebPrepareContext(ctx, client, cred, qwenWebBuildContext(turns, nil), nil, build)
	if err != nil {
		t.Fatalf("externalize: %v", err)
	}
	if len(msgs[0].Files) != 1 {
		t.Fatalf("wire files = %d, want 1 attachment", len(msgs[0].Files))
	}
	if !strings.Contains(msgs[0].Content, "Agent context attachment") {
		t.Errorf("live prompt lacks the attachment notice")
	}
	if !strings.Contains(msgs[0].Content, "CURRENT-1") {
		t.Errorf("live prompt lost the current turn")
	}
	if got := qwenWebSerializedSize(msgs); got > qwenWebContextTriggerBytes {
		t.Errorf("externalized body = %d bytes, want <= %d", got, qwenWebContextTriggerBytes)
	}
	if stsCalls.Load() != 1 || putCalls.Load() != 1 {
		t.Fatalf("STS/PUT calls = %d/%d, want 1/1", stsCalls.Load(), putCalls.Load())
	}

	// The session grows: last turn's exchange joins the history and a
	// new current turn arrives. The baked prefix must be reused.
	turns2 := append(append([]qwenWebInputMsg{}, turns...),
		qwenWebMsg("assistant", bulk("t5")), qwenWebMsg("user", "CURRENT-2"))
	msgs2, err := qwenWebPrepareContext(ctx, client, cred, qwenWebBuildContext(turns2, nil), nil, build)
	if err != nil {
		t.Fatalf("reuse turn: %v", err)
	}
	if len(msgs2[0].Files) != 1 {
		t.Fatalf("reuse wire files = %d, want 1", len(msgs2[0].Files))
	}
	if stsCalls.Load() != 1 {
		t.Errorf("STS calls after reuse = %d, want still 1 (no second upload)", stsCalls.Load())
	}
	if !strings.Contains(msgs2[0].Content, "CURRENT-2") {
		t.Errorf("reuse prompt lost the current turn")
	}
}

func TestQwenWebToolRuntimeRunawayGuard(t *testing.T) {
	rt := newQwenWebToolRuntime(map[string]bool{"ping_tool": true})
	var b strings.Builder
	b.WriteString("[TOOL CALL]\n{\"name\": \"ping_tool\", \"arguments\": {\"i\": 0}}\n[END TOOL CALL]\n")
	b.WriteString("[TOOL CALL]\n{\"name\": \"ping_tool\", \"arguments\": {\"i\": 0}}\n[END TOOL CALL]\n") // exact duplicate
	for i := 1; i <= 24; i++ {
		fmt.Fprintf(&b, "[TOOL CALL]\n{\"name\": \"ping_tool\", \"arguments\": {\"i\": %d}}\n[END TOOL CALL]\n", i)
	}
	rt.feed(b.String())
	rt.finish()
	if len(rt.calls) != qwenWebMaxCallsPerTurn {
		t.Errorf("admitted calls = %d, want the cap %d", len(rt.calls), qwenWebMaxCallsPerTurn)
	}
	if rt.dropped != 2 {
		t.Errorf("dropped = %d, want 2 (one duplicate, one over the cap)", rt.dropped)
	}
}

func TestQwenWebOSSURL(t *testing.T) {
	tests := []struct {
		name string
		sts  qwenWebSTS
		want string
	}{
		{"bare endpoint host", qwenWebSTS{Bucket: "bkt", Endpoint: "oss-accelerate.aliyuncs.com", FilePath: "/u/f.txt"},
			"https://bkt.oss-accelerate.aliyuncs.com/u/f.txt"},
		{"endpoint as URL", qwenWebSTS{Bucket: "bkt", Endpoint: "http://127.0.0.1:9999", FilePath: "u/f.txt"},
			"http://127.0.0.1:9999/u/f.txt"},
		{"region fallback", qwenWebSTS{Bucket: "bkt", Region: "oss-ap-southeast-1", FilePath: "u/f.txt"},
			"https://bkt.oss-ap-southeast-1.aliyuncs.com/u/f.txt"},
		{"file URL fallback", qwenWebSTS{FilePath: "u/f.txt", FileURL: "https://cdn.example/u/f.txt"},
			"https://cdn.example/u/f.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qwenWebOSSURL(&tt.sts); got != tt.want {
				t.Errorf("OSS URL = %q, want %q", got, tt.want)
			}
		})
	}
}

package executor

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strings"
	"testing"
)

var qwenWebTestAllowed = map[string]bool{"get_weather": true, "get_time": true, "note": true}

const qwenWebCallJakarta = "[TOOL CALL]\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}\n[END TOOL CALL]"

func assertNoMarkerResidue(t *testing.T, prose string) {
	t.Helper()
	for _, marker := range []string{"[TOOL CALL", "[END TOOL CALL", "[TOOL RESULT", "[END TOOL RESULT", "<tool_call", "</tool_call"} {
		if strings.Contains(prose, marker) {
			t.Errorf("prose leaks marker %q: %q", marker, prose)
		}
	}
}

func TestQwenWebParseToolOutput(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantProse string // exact match after TrimSpace comparison when wantProseSub is empty
		wantSub   []string
		wantCalls []qwenWebToolCall
		wantDrop  int
	}{
		{
			name:      "plain prose untouched",
			input:     "Halo, apa kabar? Cuaca hari ini cerah.",
			wantProse: "Halo, apa kabar? Cuaca hari ini cerah.",
		},
		{
			name:      "one complete call",
			input:     qwenWebCallJakarta,
			wantProse: "",
			wantCalls: []qwenWebToolCall{{Name: "get_weather", Arguments: `{"city":"Jakarta"}`}},
		},
		{
			name:  "two parallel calls",
			input: qwenWebCallJakarta + "\n[TOOL CALL]\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Surabaya\"}}\n[END TOOL CALL]",
			wantCalls: []qwenWebToolCall{
				{Name: "get_weather", Arguments: `{"city":"Jakarta"}`},
				{Name: "get_weather", Arguments: `{"city":"Surabaya"}`},
			},
		},
		{
			name:      "prose around a call survives",
			input:     "Sebentar, saya cek dulu.\n" + qwenWebCallJakarta + "\nSudah saya panggil.",
			wantSub:   []string{"Sebentar, saya cek dulu.", "Sudah saya panggil."},
			wantCalls: []qwenWebToolCall{{Name: "get_weather", Arguments: `{"city":"Jakarta"}`}},
		},
		{
			name:      "angle bracket form accepted on the read side",
			input:     "<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}\n</tool_call>",
			wantProse: "",
			wantCalls: []qwenWebToolCall{{Name: "get_weather", Arguments: `{"city":"Jakarta"}`}},
		},
		{
			name:      "name comes from the payload, never the decoration",
			input:     "[TOOL CALL] get_time\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}\n[END TOOL CALL]",
			wantProse: "",
			wantCalls: []qwenWebToolCall{{Name: "get_weather", Arguments: `{"city":"Jakarta"}`}},
		},
		{
			name:      "arguments as a JSON string are unwrapped",
			input:     "[TOOL CALL]\n{\"name\": \"get_weather\", \"arguments\": \"{\\\"city\\\": \\\"Jakarta\\\"}\"}\n[END TOOL CALL]",
			wantProse: "",
			wantCalls: []qwenWebToolCall{{Name: "get_weather", Arguments: `{"city":"Jakarta"}`}},
		},
		{
			name:      "raw control characters in argument strings are repaired",
			input:     "[TOOL CALL]\n{\"name\": \"note\", \"arguments\": {\"text\": \"baris satu\nbaris dua\"}}\n[END TOOL CALL]",
			wantProse: "",
			wantCalls: []qwenWebToolCall{{Name: "note", Arguments: "{\"text\":\"baris satu\\nbaris dua\"}"}},
		},
		{
			name:     "broken JSON arguments drop the whole span",
			input:    "Mulai.\n[TOOL CALL]\n{\"name\": \"get_weather\", \"arguments\": {\"city\": }}\n[END TOOL CALL]\nSelesai.",
			wantSub:  []string{"Mulai.", "Selesai."},
			wantDrop: 1,
		},
		{
			name:      "undeclared tool name drops the span",
			input:     "[TOOL CALL]\n{\"name\": \"send_email\", \"arguments\": {\"to\": \"a@b.c\"}}\n[END TOOL CALL]",
			wantProse: "",
			wantDrop:  1,
		},
		{
			name:      "truncated call at end of stream drops",
			input:     "[TOOL CALL]\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jak",
			wantProse: "",
			wantDrop:  1,
		},
		{
			name:      "balanced payload at end of stream is salvaged without a closer",
			input:     "[TOOL CALL]\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}",
			wantProse: "",
			wantCalls: []qwenWebToolCall{{Name: "get_weather", Arguments: `{"city":"Jakarta"}`}},
		},
		{
			name:      "orphan closer in prose is swallowed",
			input:     "Jawaban biasa [END TOOL CALL] tanpa panggilan.",
			wantProse: "Jawaban biasa  tanpa panggilan.",
		},
		{
			name:    "quoted marker without payload stays prose, marker dropped",
			input:   "Gunakan format [TOOL CALL] untuk memanggil tool dalam sistem ini.",
			wantSub: []string{"Gunakan format", "untuk memanggil tool dalam sistem ini."},
		},
		{
			name:    "JSON object without a name key after a marker is prose",
			input:   "[TOOL CALL]\n{\"city\": \"Jakarta\", \"suhu\": 31}\n[END TOOL CALL]",
			wantSub: []string{`{"city": "Jakarta", "suhu": 31}`},
		},
		{
			name:      "fenced example is documentation, not a call",
			input:     "Contoh:\n```\n" + qwenWebCallJakarta + "\n```\nItu contohnya.",
			wantSub:   []string{"Contoh:", "Itu contohnya.", "[TOOL CALL]"},
			wantCalls: nil,
		},
		{
			name:      "faked result block is swallowed whole",
			input:     "[TOOL RESULT #1: get_weather]\nArguments: {\"city\": \"Jakarta\"}\nCerah palsu.\n[END TOOL RESULT]\nSelesai.",
			wantProse: "Selesai.",
		},
		{
			name:      "dangling partial marker at end of stream is dropped",
			input:     "Jawaban saya sampai di sini [TOOL CA",
			wantProse: "Jawaban saya sampai di sini ",
		},
		{
			name:      "markdown link mentioning tool calls is not a marker",
			input:     "Lihat [tool calls](https://example.com/docs) untuk detailnya, lalu {\"name\": \"bukan\"} di sini.",
			wantProse: "Lihat [tool calls](https://example.com/docs) untuk detailnya, lalu {\"name\": \"bukan\"} di sini.",
		},
		{
			name:      "payload fenced between the markers still parses",
			input:     "[TOOL CALL]\n```json\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}\n```\n[END TOOL CALL]",
			wantProse: "",
			wantCalls: []qwenWebToolCall{{Name: "get_weather", Arguments: `{"city":"Jakarta"}`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prose, calls, dropped := qwenWebParseToolOutput(tt.input, qwenWebTestAllowed)
			if tt.name != "fenced example is documentation, not a call" {
				assertNoMarkerResidue(t, prose)
			}
			if len(tt.wantSub) == 0 {
				if strings.TrimSpace(prose) != strings.TrimSpace(tt.wantProse) {
					t.Errorf("prose = %q, want %q", prose, tt.wantProse)
				}
			}
			for _, sub := range tt.wantSub {
				if !strings.Contains(prose, sub) {
					t.Errorf("prose %q missing %q", prose, sub)
				}
			}
			if len(calls) != len(tt.wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, tt.wantCalls)
			}
			for i, want := range tt.wantCalls {
				if calls[i] != want {
					t.Errorf("call %d = %+v, want %+v", i, calls[i], want)
				}
			}
			if dropped != tt.wantDrop {
				t.Errorf("dropped = %d, want %d", dropped, tt.wantDrop)
			}
		})
	}
}

// TestQwenWebToolScannerStreamingParity feeds every parse case through
// the scanner in small chunks and demands the collect result — the
// stream and whole-text paths must not diverge.
func TestQwenWebToolScannerStreamingParity(t *testing.T) {
	inputs := []string{
		"Sebentar.\n" + qwenWebCallJakarta + "\nSelesai.",
		qwenWebCallJakarta + "\n[TOOL CALL]\n{\"name\": \"get_time\", \"arguments\": {\"tz\": \"WIB\"}}\n[END TOOL CALL]",
		"<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}\n</tool_call>",
		"teks [END TOOL CALL] biasa",
		"[TOOL CALL]\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jak",
		"Contoh:\n```\n" + qwenWebCallJakarta + "\n```\nItu contohnya.",
	}
	for _, input := range inputs {
		wantProse, wantCalls, _ := qwenWebParseToolOutput(input, qwenWebTestAllowed)
		for _, chunk := range []int{1, 3, 17} {
			sc := newQwenWebToolScanner(qwenWebTestAllowed)
			var prose strings.Builder
			var calls []qwenWebToolCall
			for i := 0; i < len(input); i += chunk {
				end := i + chunk
				if end > len(input) {
					end = len(input)
				}
				p, c := sc.feed(input[i:end])
				prose.WriteString(p)
				calls = append(calls, c...)
			}
			p, c := sc.finish()
			prose.WriteString(p)
			calls = append(calls, c...)
			if prose.String() != wantProse {
				t.Errorf("chunk %d: prose = %q, want %q (input %q)", chunk, prose.String(), wantProse, input)
			}
			if len(calls) != len(wantCalls) {
				t.Fatalf("chunk %d: calls = %v, want %v (input %q)", chunk, calls, wantCalls, input)
			}
			for i := range wantCalls {
				if calls[i] != wantCalls[i] {
					t.Errorf("chunk %d: call %d = %+v, want %+v", chunk, i, calls[i], wantCalls[i])
				}
			}
		}
	}
}

func qwenWebWeatherTools(t *testing.T) []qwenWebToolDef {
	t.Helper()
	var tools []qwenWebToolDef
	raw := `[{"type":"function","function":{"name":"get_weather","description":"Get the current weather for a city.","parameters":{"type":"object","properties":{"city":{"type":"string"},"units":{"type":"string","enum":["metric","imperial"]}},"required":["city"]}}}]`
	if err := json.Unmarshal([]byte(raw), &tools); err != nil {
		t.Fatalf("tools fixture: %v", err)
	}
	return tools
}

// qwenWebMsgJSON builds an input message through the real JSON envelope
// parsing, the shape clients actually send.
func qwenWebMsgJSON(t *testing.T, raw string) qwenWebInputMsg {
	t.Helper()
	var m qwenWebInputMsg
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("message fixture %s: %v", raw, err)
	}
	return m
}

func TestQwenWebToolContext(t *testing.T) {
	tools := qwenWebWeatherTools(t)
	tests := []struct {
		name     string
		tools    []qwenWebToolDef
		choice   string
		wantNil  bool
		wantSubs []string
	}{
		{"no tools is the v1 path", nil, "", true, nil},
		{"tool_choice none disables", tools, `"none"`, true, nil},
		{"auto renders the protocol", tools, "", false, []string{"# Tools", "- get_weather{ city: string; units?: \"metric\" | \"imperial\" }", "[TOOL CALL]", "[END TOOL CALL]", "Get the current weather for a city."}},
		{"required adds the must-call rule", tools, `"required"`, false, []string{"MUST call at least one tool"}},
		{"forced names the tool", tools, `{"type":"function","function":{"name":"get_weather"}}`, false, []string{"MUST call the tool `get_weather` first"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := qwenWebNewToolContext(tt.tools, jsontext.Value(tt.choice))
			if tt.wantNil {
				if ctx != nil {
					t.Fatalf("ctx = %+v, want nil", ctx)
				}
				return
			}
			if ctx == nil {
				t.Fatal("ctx = nil, want protocol")
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(ctx.protocol, sub) {
					t.Errorf("protocol missing %q:\n%s", sub, ctx.protocol)
				}
			}
			if !ctx.allowed["get_weather"] {
				t.Errorf("allowed = %v", ctx.allowed)
			}
		})
	}
}

func TestQwenWebFlattenToolContent(t *testing.T) {
	tools := qwenWebWeatherTools(t)
	ctx := qwenWebNewToolContext(tools, nil)

	t.Run("lone user message keeps its text bare under the protocol head", func(t *testing.T) {
		in := []qwenWebInputMsg{{Role: "user", Content: jsontext.Value(`"Cuaca di Jakarta sekarang?"`)}}
		got := qwenWebFlattenToolContent(in, ctx)
		if !strings.HasPrefix(got, "# Tools") {
			t.Errorf("transcript does not start with the protocol:\n%s", got)
		}
		if !strings.HasSuffix(got, "Cuaca di Jakarta sekarang?") {
			t.Errorf("transcript does not end with the bare question:\n%s", got)
		}
		if strings.Contains(got, "User: Cuaca") {
			t.Errorf("lone user message got a label:\n%s", got)
		}
	})

	t.Run("calls and results fold with ordinals and echoed arguments", func(t *testing.T) {
		in := []qwenWebInputMsg{
			qwenWebMsgJSON(t, `{"role":"user","content":"Cuaca di Jakarta dan Surabaya?"}`),
			qwenWebMsgJSON(t, `{"role":"assistant","content":"","tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Jakarta\"}"}},
				{"id":"call_b","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Surabaya\"}"}}
			]}`),
			qwenWebMsgJSON(t, `{"role":"tool","tool_call_id":"call_b","content":"Hujan, 27°C."}`),
			qwenWebMsgJSON(t, `{"role":"tool","tool_call_id":"call_a","content":"Cerah, 31°C."}`),
			qwenWebMsgJSON(t, `{"role":"user","content":"Perlu payung di Jakarta?"}`),
		}
		got := qwenWebFlattenToolContent(in, ctx)
		for _, sub := range []string{
			"[TOOL CALL #1]", "[TOOL CALL #2]",
			"[TOOL RESULT #2: get_weather]", "[TOOL RESULT #1: get_weather]",
			`Arguments: {"city":"Surabaya"}`, `Arguments: {"city":"Jakarta"}`,
			"Hujan, 27°C.", "Cerah, 31°C.", "User: Perlu payung di Jakarta?",
		} {
			if !strings.Contains(got, sub) {
				t.Errorf("transcript missing %q:\n%s", sub, got)
			}
		}
		// The Surabaya result must be the one echoing Surabaya's arguments:
		// check the pairing by ordering within the result block.
		idx := strings.Index(got, "[TOOL RESULT #2: get_weather]")
		seg := got[idx:]
		if !strings.HasPrefix(seg, "[TOOL RESULT #2: get_weather]\nArguments: {\"city\":\"Surabaya\"}\nHujan, 27°C.") {
			t.Errorf("result #2 block pairs the wrong arguments:\n%s", seg)
		}
	})

	t.Run("result with an unknown id renders unnumbered without arguments", func(t *testing.T) {
		in := []qwenWebInputMsg{
			qwenWebMsgJSON(t, `{"role":"user","content":"lanjutkan"}`),
			qwenWebMsgJSON(t, `{"role":"tool","tool_call_id":"call_zzz","name":"get_weather","content":"Cerah."}`),
		}
		got := qwenWebFlattenToolContent(in, ctx)
		if !strings.Contains(got, "[TOOL RESULT: get_weather]\nCerah.\n[END TOOL RESULT]") {
			t.Errorf("unnumbered result block wrong:\n%s", got)
		}
		seg := got[strings.Index(got, "[TOOL RESULT: get_weather]"):]
		if strings.Contains(seg, "Arguments:") {
			t.Errorf("unknown call must not gain echoed arguments:\n%s", seg)
		}
	})

	t.Run("empty tool result renders as (empty)", func(t *testing.T) {
		in := []qwenWebInputMsg{
			qwenWebMsgJSON(t, `{"role":"assistant","content":"","tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Jakarta\"}"}}
			]}`),
			qwenWebMsgJSON(t, `{"role":"tool","tool_call_id":"call_a","content":""}`),
		}
		got := qwenWebFlattenToolContent(in, ctx)
		if !strings.Contains(got, "[TOOL RESULT #1: get_weather]\nArguments: {\"city\":\"Jakarta\"}\n(empty)\n[END TOOL RESULT]") {
			t.Errorf("empty result block wrong:\n%s", got)
		}
	})
}

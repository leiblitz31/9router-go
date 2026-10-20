package proxy

import (
	"testing"
)

func TestIsZeroContentResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "empty choices",
			body: `{"choices":[]}`,
			want: true,
		},
		{
			name: "blank content with finish_reason stop",
			body: `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`,
			want: false,
		},
		{
			name: "blank content with finish_reason length",
			body: `{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`,
			want: false,
		},
		{
			name: "in_progress is zero content",
			body: `{"choices":[{"message":{"content":""},"finish_reason":"in_progress"}]}`,
			want: true,
		},
		{
			name: "incomplete is zero content",
			body: `{"choices":[{"message":{"content":""},"finish_reason":"incomplete"}]}`,
			want: true,
		},
		{
			name: "no finish reason with blank content is zero content",
			body: `{"choices":[{"message":{"content":""}}]}`,
			want: true,
		},
		{
			name: "tool_calls present is not zero content",
			body: `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"search"}}]},"finish_reason":"in_progress"}]}`,
			want: false,
		},
		{
			name: "function_call present is not zero content",
			body: `{"choices":[{"message":{"content":"","function_call":{"name":"search","arguments":"{}"}},"finish_reason":"in_progress"}]}`,
			want: false,
		},
		{
			name: "reasoning_content present is not zero content",
			body: `{"choices":[{"message":{"content":"","reasoning_content":"thought process"},"finish_reason":"in_progress"}]}`,
			want: false,
		},
		{
			name: "content text present is not zero content",
			body: `{"choices":[{"message":{"content":"Hello world"},"finish_reason":"stop"}]}`,
			want: false,
		},
		{
			name: "Anthropic content with a text block",
			body: `{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`,
			want: false,
		},
		{
			name: "Anthropic content with a tool_use block",
			body: `{"content":[{"type":"tool_use","id":"toolu_1","name":"bash"}],"stop_reason":"in_progress"}`,
			want: false,
		},
		{
			name: "Anthropic empty content with stop_reason max_tokens",
			body: `{"content":[],"stop_reason":"max_tokens"}`,
			want: false,
		},
		{
			name: "Anthropic empty content with no stop_reason",
			body: `{"content":[]}`,
			want: true,
		},
		{
			name: "Anthropic empty content with stop_reason in_progress",
			body: `{"content":[],"stop_reason":"in_progress"}`,
			want: true,
		},
		{
			name: "Anthropic empty content with stop_reason incomplete",
			body: `{"content":[],"stop_reason":"incomplete"}`,
			want: true,
		},
		{
			name: "array reasoning_content counts as no reasoning (zero content)",
			body: `{"choices":[{"message":{"content":"","reasoning_content":[{"type":"text","text":"thinking"}]},"finish_reason":"in_progress"}]}`,
			want: true,
		},
		{
			name: "numeric finish_reason is not normal finish (zero content)",
			body: `{"choices":[{"message":{"content":""},"finish_reason":5}]}`,
			want: true,
		},
		{
			name: "empty object function_call counts as tool call (not zero content)",
			body: `{"choices":[{"message":{"content":"","function_call":{}},"finish_reason":"in_progress"}]}`,
			want: false,
		},
		{
			name: "message preferred over delta when both present (zero content)",
			body: `{"choices":[{"message":{"content":""},"delta":{"content":"text in delta"},"finish_reason":"in_progress"}]}`,
			want: true,
		},
		{
			name: "a non-JSON body",
			body: `upstream error 500`,
			want: false,
		},
		{
			name: "empty body",
			body: ``,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsZeroContentResponse([]byte(tt.body))
			if got != tt.want {
				t.Errorf("IsZeroContentResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

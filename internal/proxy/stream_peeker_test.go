package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStreamContentPeeker_EmitsContentAndCommits(t *testing.T) {
	rec := httptest.NewRecorder()
	peeker := NewStreamContentPeeker(rec)
	peeker.Header().Set("Content-Type", "text/event-stream")

	// First chunk: empty role block
	_, _ = peeker.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	if rec.Code != http.StatusOK && rec.Body.Len() > 0 {
		t.Fatalf("peeker should not have committed yet")
	}

	// Second chunk: real content
	_, _ = peeker.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	if rec.Body.Len() == 0 {
		t.Fatalf("peeker should have committed once content arrived")
	}

	err := peeker.Finish()
	if err != nil {
		t.Fatalf("unexpected error on finish: %v", err)
	}
}

func TestStreamContentPeeker_ZeroContentReturnsError(t *testing.T) {
	rec := httptest.NewRecorder()
	peeker := NewStreamContentPeeker(rec)

	// Stream that ends with role + finish_reason only (no content)
	_, _ = peeker.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	_, _ = peeker.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
	_, _ = peeker.Write([]byte("data: [DONE]\n\n"))

	err := peeker.Finish()
	if !errors.Is(err, ErrStreamingZeroContent) {
		t.Fatalf("expected ErrStreamingZeroContent, got %v", err)
	}
	if rec.Body.Len() > 0 {
		t.Fatalf("zero-content stream should not have committed any body to underlying recorder")
	}
}

func TestStreamContentPeeker_ClaudeThinkingCommits(t *testing.T) {
	// Port of the #1028 cases from upstream's
	// TestStreamContentPeeker_ReasoningContentCommits. The reasoning-text
	// cases of that test are NOT ported: they pin upstream #845 (reasoning
	// streams count as non-empty), which the fork has never taken — here a
	// reasoning-only stream stays zero-content so combo fallback can retry.
	tests := []struct {
		name         string
		chunks       []string
		expectCommit bool
		expectErr    error
	}{
		{
			name:         "Claude content_block_start redacted_thinking commits",
			chunks:       []string{"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"redacted_bytes\"}}\n\n"},
			expectCommit: true,
			expectErr:    nil,
		},
		{
			name:         "Claude content_block_start thinking commits",
			chunks:       []string{"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n"},
			expectCommit: true,
			expectErr:    nil,
		},
		{
			name:         "Claude signature_delta commits",
			chunks:       []string{"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig123\"}}\n\n"},
			expectCommit: true,
			expectErr:    nil,
		},
		{
			name:         "Silent stream without content fails with ErrStreamingZeroContent",
			chunks:       []string{"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n", "data: [DONE]\n\n"},
			expectCommit: false,
			expectErr:    ErrStreamingZeroContent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			peeker := NewStreamContentPeeker(rec)
			peeker.Header().Set("Content-Type", "text/event-stream")

			for _, chunk := range tc.chunks {
				_, _ = peeker.Write([]byte(chunk))
			}

			err := peeker.Finish()
			if !errors.Is(err, tc.expectErr) {
				t.Fatalf("expected error %v, got %v", tc.expectErr, err)
			}
			if tc.expectCommit && rec.Body.Len() == 0 {
				t.Fatalf("expected stream to commit and write body, but recorder body is empty")
			}
			if !tc.expectCommit && rec.Body.Len() > 0 {
				t.Fatalf("expected stream not to commit, but got recorder body: %s", rec.Body.String())
			}
		})
	}
}

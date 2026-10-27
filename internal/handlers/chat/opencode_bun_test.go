package chat

import "testing"

func TestIsOpencodeFreeLane(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{provider: "opencode", want: true},
		{provider: "oc", want: true},
		{provider: "opencode-zen", want: false},
		{provider: "opencode-go", want: false},
		{provider: "cc", want: false},
		{provider: "cf", want: false},
		{provider: "ps", want: false},
		{provider: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := isOpencodeFreeLane(tt.provider); got != tt.want {
				t.Errorf("isOpencodeFreeLane(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

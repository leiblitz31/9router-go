package executor

import "testing"

func TestOpencodeDebugHash(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{
			name:  "empty body",
			input: []byte{},
			want:  "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:  "short body",
			input: []byte("abc"),
			want:  "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		},
		{
			name:  "sentence body",
			input: []byte("hello world"),
			want:  "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := opencodeDebugHash(tt.input); got != tt.want {
				t.Errorf("opencodeDebugHash() = %q, want %q", got, tt.want)
			}
		})
	}
}

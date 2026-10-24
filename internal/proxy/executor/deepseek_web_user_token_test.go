package executor

import "testing"

// deepSeekUserToken must agree with the browser-login reader about what
// counts as a credential, and with upstream decolua/9router#5182 about
// the JSON wrapper: a wrapper only counts when its value is a string of
// 20+ chars. A logged-out page holds {"value":null,"__version":"0"}; the
// old code fell back to that raw JSON string as the token, so the
// dashboard probe sent a Bearer value the identity probe rejects.
func TestDeepSeekUserToken(t *testing.T) {
	const valid = "tfWDPBARqgRC/+KfPOmhLFeW78PdKOKWiI3ro8MaP9hvRd1IoKZrMSd2L5jEaUZN"
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "the site's own object form",
			raw:  `{"value":"` + valid + `","__version":"0"}`,
			want: valid,
		},
		{
			name: "a logged-out page holds a null, not a token",
			raw:  `{"value":null,"__version":"0"}`,
			want: "",
		},
		{
			name: "a wrapped empty string is not a token",
			raw:  `{"value":"","__version":"0"}`,
			want: "",
		},
		{
			name: "a wrapped value shorter than 20 chars is not a token",
			raw:  `{"value":"short","__version":"0"}`,
			want: "",
		},
		{
			name: "a wrapped non-string value is not a token",
			raw:  `{"value":123,"__version":"0"}`,
			want: "",
		},
		{
			name: "malformed JSON starting with a brace is not a token",
			raw:  `{"value":}`,
			want: "",
		},
		{
			name: "a bare JSON string unwraps",
			raw:  `"the-token"`,
			want: "the-token",
		},
		{
			name: "a plain value trims",
			raw:  "  the-token\n",
			want: "the-token",
		},
		{
			name: "an empty value",
			raw:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deepSeekUserToken(tt.raw); got != tt.want {
				t.Errorf("deepSeekUserToken(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

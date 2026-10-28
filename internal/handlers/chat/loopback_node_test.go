package chat

import "testing"

func TestIsLoopbackBaseURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"loopback v4 with port and path", "http://127.0.0.1:20232/v1", true},
		{"loopback v4 other octet", "http://127.0.0.2:8080", true},
		{"loopback v6", "http://[::1]:20232/v1", true},
		{"localhost http", "http://localhost:20232/v1", true},
		{"localhost https no port", "https://localhost/v1", true},
		{"localhost uppercase", "http://LOCALHOST:20232/v1", true},
		{"private lan is not loopback", "http://192.168.1.10:20232/v1", false},
		{"private 10 is not loopback", "http://10.0.0.5/v1", false},
		{"tailscale cgnat is not loopback", "http://100.77.22.88/v1", false},
		{"public host", "https://api.clevecode.com/v1", false},
		{"empty", "", false},
		{"unparseable", "not a url", false},
		{"no scheme has no host", "127.0.0.1:20232/v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLoopbackBaseURL(tc.raw); got != tc.want {
				t.Errorf("isLoopbackBaseURL(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestIsLoopbackCustomNodeNilGuards(t *testing.T) {
	var h *ChatHandler
	if h.isLoopbackCustomNode("cb") {
		t.Error("nil handler must not report a loopback node")
	}
	h = &ChatHandler{}
	if h.isLoopbackCustomNode("cb") {
		t.Error("nil repo must not report a loopback node")
	}
	if h.isLoopbackCustomNode("") {
		t.Error("empty provider must not report a loopback node")
	}
}

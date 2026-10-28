package proxy

import (
	"testing"
)

func TestNewSafeTransportWithProxySpec(t *testing.T) {
	tests := []struct {
		name     string
		proxyURL string
		wantKind transportProxyKind
		wantURL  string
	}{
		{
			name:     "explicit proxy registers kind and url",
			proxyURL: "http://127.0.0.1:18080",
			wantKind: proxyKindExplicit,
			wantURL:  "http://127.0.0.1:18080",
		},
		{
			name:     "empty proxy registers no proxy",
			proxyURL: "",
			wantKind: proxyKindNone,
			wantURL:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewSafeTransportWithProxySpec(tt.proxyURL, false, nil)
			if tr == nil {
				t.Fatal("expected non-nil transport")
			}
			if tr.DialTLSContext == nil {
				t.Error("expected DialTLSContext to be set")
			}
			got := lookupTransportProxy(tr)
			if got.kind != tt.wantKind {
				t.Errorf("proxy kind = %v, want %v", got.kind, tt.wantKind)
			}
			if got.url != tt.wantURL {
				t.Errorf("proxy url = %q, want %q", got.url, tt.wantURL)
			}
		})
	}
}

package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// roundTripperFunc adapts a function to http.RoundTripper for FallbackTransport
// tests, so each case can script the base and direct transports independently.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func okResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestFallbackTransportPrimaryErrorOnSSRF pins the error a caller sees when
// the direct fallback cannot even dial: the primary proxy failure is the
// actionable error, and the SSRF refusal of the fallback must not replace it.
// In a sandbox whose DNS maps every external host to a sentinel range, the
// direct dial is refused by design, so reporting it would bury the real
// failure (a stalled or refused proxy) behind a guard verdict.
func TestFallbackTransportPrimaryErrorOnSSRF(t *testing.T) {
	directSSRF := &url.Error{
		Op:  "Post",
		URL: "https://upstream.invalid/chat",
		Err: ErrSSRFBlocked,
	}
	proxy403 := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"X-Proxy-Error": []string{"refused"}},
		Body:       io.NopCloser(strings.NewReader("forbidden")),
	}

	tests := []struct {
		name string
		base roundTripperFunc
		// directErr is returned by the direct transport; directResp is its
		// response when directErr is nil.
		directErr  error
		directResp *http.Response
		wantBody   string
		wantErr    string
		wantSSRF   bool
	}{
		{
			name:      "base proxy error plus direct SSRF returns primary error",
			base:      func(*http.Request) (*http.Response, error) { return nil, errProxyRefusal },
			directErr: directSSRF,
			wantErr:   "proxyconnect tcp: connection refused",
			wantSSRF:  false,
		},
		{
			name:       "base proxy error plus direct success returns direct response",
			base:       func(*http.Request) (*http.Response, error) { return nil, errProxyRefusal },
			directResp: okResponse("direct-ok"),
			wantBody:   "direct-ok",
		},
		{
			name:      "base proxy error plus direct non-SSRF error returns direct error",
			base:      func(*http.Request) (*http.Response, error) { return nil, errProxyRefusal },
			directErr: errors.New("connection reset by peer"),
			wantErr:   "connection reset by peer",
		},
		{
			name:       "base success skips fallback",
			base:       func(*http.Request) (*http.Response, error) { return okResponse("base-ok"), nil },
			directResp: okResponse("direct-ok"),
			wantBody:   "base-ok",
		},
		{
			name:      "base 403 response keeps old behavior on direct SSRF",
			base:      func(*http.Request) (*http.Response, error) { return proxy403, nil },
			directErr: directSSRF,
			wantErr:   "request to private/reserved IP blocked by SSRF protection",
			wantSSRF:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var directCalls atomic.Int64
			direct := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				directCalls.Add(1)
				return tt.directResp, tt.directErr
			})
			tr := &FallbackTransport{Base: tt.base, Direct: direct}

			req := httptest.NewRequest(http.MethodGet, "https://upstream.invalid/chat", nil)
			resp, err := tr.RoundTrip(req)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("RoundTrip() error = nil, want containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("RoundTrip() error = %v, want containing %q", err, tt.wantErr)
				}
				if errors.Is(err, ErrSSRFBlocked) != tt.wantSSRF {
					t.Errorf("errors.Is(err, ErrSSRFBlocked) = %v, want %v (err = %v)",
						errors.Is(err, ErrSSRFBlocked), tt.wantSSRF, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("RoundTrip() error = %v, want nil", err)
			}
			if resp == nil {
				t.Fatal("RoundTrip() response = nil, want a response")
			}
			defer resp.Body.Close()
			body, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				t.Fatalf("reading response body: %v", readErr)
			}
			if string(body) != tt.wantBody {
				t.Errorf("RoundTrip() body = %q, want %q", body, tt.wantBody)
			}
			if tt.name == "base success skips fallback" && directCalls.Load() != 0 {
				t.Errorf("direct transport calls = %d, want 0", directCalls.Load())
			}
		})
	}
}

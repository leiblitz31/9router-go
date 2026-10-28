package chat

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"

	"9router/proxy/internal/proxy/executor"
)

// isGREASE reports whether c is a GREASE cipher value (RFC 8701): both
// bytes equal with low nibble 0xA (0x0A0A, 0x1A1A, …, 0xFAFA). uTLS
// replaces the spec's 0x0A0A placeholder with a random GREASE value per
// handshake, so a captured Chrome hello never equals the spec list
// byte-for-byte in that slot — the comparison must normalize it away.
func isGREASE(c uint16) bool {
	return c>>8 == c&0xff && c&0x0f == 0x0a
}

// stripGREASE drops GREASE values from a cipher list.
func stripGREASE(in []uint16) []uint16 {
	out := make([]uint16, 0, len(in))
	for _, c := range in {
		if !isGREASE(c) {
			out = append(out, c)
		}
	}
	return out
}

// parseClientHelloCiphers extracts the cipher-suite list from one TLS
// ClientHello record. It reads exactly one record: the uTLS stack emits the
// hello as a single record, so a second record means the capture is corrupt,
// not fragmented.
func parseClientHelloCiphers(record []byte) ([]uint16, error) {
	if len(record) < 5 || record[0] != 0x16 {
		return nil, errNotClientHello
	}
	hello := record[5:]
	if len(hello) < 4 || hello[0] != 0x01 {
		return nil, errNotClientHello
	}
	// handshake header (4) + legacy version (2) + random (32)
	pos := 4 + 2 + 32
	if len(hello) < pos+1 {
		return nil, errNotClientHello
	}
	sessionLen := int(hello[pos])
	pos++
	pos += sessionLen
	if len(hello) < pos+2 {
		return nil, errNotClientHello
	}
	suitesLen := int(hello[pos])<<8 | int(hello[pos+1])
	pos += 2
	if suitesLen%2 != 0 || len(hello) < pos+suitesLen {
		return nil, errNotClientHello
	}
	suites := make([]uint16, 0, suitesLen/2)
	for i := 0; i < suitesLen; i += 2 {
		suites = append(suites, uint16(hello[pos+i])<<8|uint16(hello[pos+i+1]))
	}
	return suites, nil
}

type errString string

func (e errString) Error() string { return string(e) }

const errNotClientHello = errString("not a ClientHello record")

// captureServer records every ClientHello cipher list it receives, then
// closes the connection: the handshake never completes, so the client sees
// an error — the test asserts on the captured hello, not on HTTP success.
type captureServer struct {
	ln       net.Listener
	mu       sync.Mutex
	captured [][]uint16
}

func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &captureServer{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *captureServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	length := int(header[3])<<8 | int(header[4])
	if length <= 0 || length > 1<<16 {
		return
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return
	}
	record := append(header, payload...)
	suites, err := parseClientHelloCiphers(record)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.captured = append(s.captured, suites)
	s.mu.Unlock()
}

func (s *captureServer) wait(t *testing.T) [][]uint16 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := append([][]uint16(nil), s.captured...)
		s.mu.Unlock()
		if len(got) > 0 {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no ClientHello captured")
	return nil
}

// connectProxy is a minimal HTTP CONNECT proxy: it dials the requested
// target and pipes bytes both ways.
func newConnectProxy(t *testing.T) *httptest.Server {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "connect only", http.StatusMethodNotAllowed)
			return
		}
		target, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			target.Close()
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			target.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = bufrw.Flush()
		go func() { _, _ = io.Copy(target, conn); target.Close(); conn.Close() }()
		_, _ = io.Copy(conn, target)
		target.Close()
		conn.Close()
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

func TestGetClientForConnection_OpencodeFreeLaneBunHelloViaHTTPProxy(t *testing.T) {
	bunSpec, err := executor.OpencodeBunSpecFactory()()
	if err != nil {
		t.Fatalf("bun spec: %v", err)
	}
	chromeSpec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		t.Fatalf("chrome spec: %v", err)
	}

	tests := []struct {
		name     string
		provider string
		want     []uint16
	}{
		{
			name:     "opencode free lane sends Bun hello through http proxy",
			provider: "opencode",
			want:     bunSpec.CipherSuites,
		},
		{
			name:     "other provider sends Chrome hello through http proxy",
			provider: "openai",
			want:     chromeSpec.CipherSuites,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := newCaptureServer(t)
			proxy := newConnectProxy(t)

			h := &ChatHandler{Client: &http.Client{}}
			connData := &ConnectionData{
				ConnectionProxyEnabled: true,
				ConnectionProxyURL:     proxy.URL,
			}
			client, err := h.getClientForConnection(connData, tt.provider)
			if err != nil {
				t.Fatalf("getClientForConnection: %v", err)
			}
			client.Timeout = 10 * time.Second
			// The handshake never completes by design; only the hello matters.
			resp, err := client.Get("https://" + target.ln.Addr().String())
			if err == nil {
				resp.Body.Close()
			}
			for _, got := range target.wait(t) {
				// The wire GREASE slot is randomized per handshake, so
				// compare GREASE-stripped lists; when the spec carries a
				// GREASE placeholder the wire hello must carry a GREASE
				// value in its place.
				if !reflect.DeepEqual(stripGREASE(got), stripGREASE(tt.want)) {
					t.Errorf("ClientHello ciphers = %v, want %v (GREASE-stripped)", stripGREASE(got), stripGREASE(tt.want))
				}
				wantGREASE := false
				for _, c := range tt.want {
					if isGREASE(c) {
						wantGREASE = true
					}
				}
				if wantGREASE {
					if len(got) == 0 || !isGREASE(got[0]) {
						t.Errorf("ClientHello first cipher missing GREASE: got %v", got)
					}
				}
			}
		})
	}
}

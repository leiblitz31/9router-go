package petaniproxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// TestE2EGatewayRotation is the end-to-end functional test: starts a real
// gateway server, routes traffic through fake upstream proxies, verifies
// rotation, header sanitization, and pool stats.
func TestE2EGatewayRotation(t *testing.T) {
	// Fake upstream proxies identifying themselves
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "from-up1")
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "from-up2")
	}))
	defer up2.Close()

	u1, _ := url.Parse(up1.URL)
	u2, _ := url.Parse(up2.URL)
	p1, _ := strconv.Atoi(u1.Port())
	p2, _ := strconv.Atoi(u2.Port())
	proxies := []Proxy{
		{IP: "127.0.0.1", Port: p1, Protocol: "http"},
		{IP: "127.0.0.1", Port: p2, Protocol: "http"},
	}

	srv := NewServer("127.0.0.1:0", WithProxies(proxies))
	if err := srv.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	proxyURL, _ := url.Parse("http://" + srv.Addr())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	// Rotation: 6 requests must hit both upstreams
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		resp, err := client.Get("http://example.test/")
		if err != nil {
			t.Fatalf("request %d error: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		seen[string(body)] = true
	}
	if !seen["from-up1"] || !seen["from-up2"] {
		t.Fatalf("rotation failed, seen=%v", seen)
	}

	// Header sanitizer: Proxy-Connection must not leak upstream
	up3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Connection") != "" {
			fmt.Fprint(w, "LEAKED")
			return
		}
		fmt.Fprint(w, "clean")
	}))
	defer up3.Close()
	u3, _ := url.Parse(up3.URL)
	p3, _ := strconv.Atoi(u3.Port())
	srv.SetProxies([]Proxy{{IP: "127.0.0.1", Port: p3, Protocol: "http"}})
	time.Sleep(100 * time.Millisecond)

	req, _ := http.NewRequest("GET", "http://example.test/", nil)
	req.Header.Set("Proxy-Connection", "keep-alive")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("sanitizer request error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "clean" {
		t.Fatalf("hop-by-hop header leaked upstream, body=%q", body)
	}

	// Pool stats sanity
	stats := srv.Pool().Stats()
	if stats.PoolSize < 0 || stats.TotalRouted == 0 {
		t.Fatalf("bad pool stats: %+v", stats)
	}
}

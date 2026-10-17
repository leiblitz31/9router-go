package petaniproxy

import (
	"testing"
	"time"
)

func testProxies() []Proxy {
	return []Proxy{
		{IP: "10.0.0.1", Port: 8080},
		{IP: "10.0.0.2", Port: 8080},
		{IP: "10.0.0.3", Port: 8080},
	}
}

func TestPoolRotationOrder(t *testing.T) {
	pm := NewPoolManager(testProxies())
	want := []string{"10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.3:8080", "10.0.0.1:8080", "10.0.0.2:8080"}
	for i, w := range want {
		got, ok := pm.Next("")
		if !ok {
			t.Fatalf("Next() #%d: ok=false, want proxy %s", i, w)
		}
		if got.Addr() != w {
			t.Errorf("Next() #%d = %s, want %s", i, got.Addr(), w)
		}
	}
	if st := pm.Stats(); st.TotalRouted != uint64(len(want)) {
		t.Errorf("TotalRouted = %d, want %d", st.TotalRouted, len(want))
	}
}

func TestPoolForceRotate(t *testing.T) {
	pm := NewPoolManager(testProxies())
	// Mirrors server.py force_rotate: index++ then return slot.
	got, ok := pm.ForceRotate()
	if !ok {
		t.Fatal("ForceRotate() ok=false")
	}
	if got.Addr() != "10.0.0.2:8080" {
		t.Errorf("ForceRotate() = %s, want 10.0.0.2:8080", got.Addr())
	}
	got, _ = pm.ForceRotate()
	if got.Addr() != "10.0.0.3:8080" {
		t.Errorf("ForceRotate() #2 = %s, want 10.0.0.3:8080", got.Addr())
	}
	if st := pm.Stats(); st.TotalRouted != 0 {
		t.Errorf("ForceRotate must not count as routed request, got %d", st.TotalRouted)
	}
}

func TestPoolGetRandom(t *testing.T) {
	proxies := testProxies()
	pm := NewPoolManager(proxies)
	for i := 0; i < 30; i++ {
		got, ok := pm.GetRandom()
		if !ok {
			t.Fatal("GetRandom() ok=false on non-empty pool")
		}
		if !proxyInPool(got, proxies) {
			t.Fatalf("GetRandom() = %v, not in pool", got)
		}
	}
}

func TestPoolEmpty(t *testing.T) {
	pm := NewPoolManager(nil)
	if _, ok := pm.Next(""); ok {
		t.Error("Next() on empty pool: ok=true, want false")
	}
	if _, ok := pm.Next("sess"); ok {
		t.Error("Next(sess) on empty pool: ok=true, want false")
	}
	if _, ok := pm.GetRandom(); ok {
		t.Error("GetRandom() on empty pool: ok=true, want false")
	}
	if _, ok := pm.ForceRotate(); ok {
		t.Error("ForceRotate() on empty pool: ok=true, want false")
	}
	st := pm.Stats()
	if st.PoolSize != 0 || st.ActiveProxy != "" || st.SuccessRatePercent != 100.0 {
		t.Errorf("empty pool stats = %+v", st)
	}
}

func TestPoolSetProxiesResets(t *testing.T) {
	pm := NewPoolManager(testProxies())
	pm.Next("")
	pm.Next("")
	pm.SetProxies([]Proxy{{IP: "9.9.9.9", Port: 3128}})
	got, ok := pm.Next("")
	if !ok || got.Addr() != "9.9.9.9:3128" {
		t.Errorf("after SetProxies, Next() = %v,%v; want 9.9.9.9:3128,true", got, ok)
	}
	if n := pm.Size(); n != 1 {
		t.Errorf("Size() = %d, want 1", n)
	}
	if all := pm.GetAll(); len(all) != 1 || all[0].IP != "9.9.9.9" {
		t.Errorf("GetAll() = %v", all)
	}
}

func TestStickySessionPinning(t *testing.T) {
	pm := NewPoolManager(testProxies())

	first, _ := pm.Next("sess-A")
	second, _ := pm.Next("sess-A")
	if first != second {
		t.Errorf("sticky session not pinned: %v then %v", first, second)
	}
	other, _ := pm.Next("sess-B")
	if other == first {
		t.Errorf("different session got same proxy %v; want rotation", other)
	}
	again, _ := pm.Next("sess-A")
	if again != first {
		t.Errorf("sess-A lost pin: got %v want %v", again, first)
	}
	if st := pm.Stats(); st.ActiveStickySessions != 2 {
		t.Errorf("ActiveStickySessions = %d, want 2", st.ActiveStickySessions)
	}
}

func TestStickySessionExpiry(t *testing.T) {
	pm := NewPoolManager(testProxies())
	pm.SetSessionTTL(30 * time.Millisecond)

	first, _ := pm.Next("sess")
	if first.Addr() != "10.0.0.1:8080" {
		t.Fatalf("first Next(sess) = %s, want 10.0.0.1:8080", first.Addr())
	}
	time.Sleep(60 * time.Millisecond)
	second, ok := pm.Next("sess")
	if !ok {
		t.Fatal("Next(sess) after expiry: ok=false")
	}
	// Rotation index continued while the pin was held: next slot is .2.
	if second.Addr() != "10.0.0.2:8080" {
		t.Errorf("Next(sess) after expiry = %s, want 10.0.0.2:8080 (fresh rotation)", second.Addr())
	}
}

func TestStickySessionEvictedProxy(t *testing.T) {
	pm := NewPoolManager(testProxies())
	pinned, _ := pm.Next("sess")
	if pinned.Addr() != "10.0.0.1:8080" {
		t.Fatalf("pinned = %s", pinned.Addr())
	}
	// Evict the pinned proxy from the pool: session must re-pin to a live one.
	pm.SetProxies([]Proxy{{IP: "10.0.0.2", Port: 8080}, {IP: "10.0.0.3", Port: 8080}})
	got, _ := pm.Next("sess")
	if got.Addr() == "10.0.0.1:8080" {
		t.Errorf("session stayed pinned to evicted proxy %s", got.Addr())
	}
}

func TestStickySessionPurged(t *testing.T) {
	pm := NewPoolManager(testProxies())
	pm.SetSessionTTL(20 * time.Millisecond)
	pm.Next("s1")
	if st := pm.Stats(); st.ActiveStickySessions != 1 {
		t.Fatalf("ActiveStickySessions = %d, want 1", st.ActiveStickySessions)
	}
	time.Sleep(40 * time.Millisecond)
	pm.Next("") // any Next purges expired sessions
	if st := pm.Stats(); st.ActiveStickySessions != 0 {
		t.Errorf("expired session not purged: %d active", st.ActiveStickySessions)
	}
}

func TestPoolStats(t *testing.T) {
	pm := NewPoolManager([]Proxy{
		{IP: "10.0.0.1", Port: 8080, Country: "Indonesia", CountryCode: "ID"},
		{IP: "10.0.0.2", Port: 8080},
	})
	pm.Next("")
	pm.Next("")
	pm.MarkResult(true)
	pm.MarkResult(false)

	st := pm.Stats()
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"PoolSize", st.PoolSize, 2},
		{"TotalRouted", st.TotalRouted, uint64(2)},
		{"Successful", st.Successful, uint64(1)},
		{"Failed", st.Failed, uint64(1)},
		{"SuccessRatePercent", st.SuccessRatePercent, 50.0},
		{"CurrentIndex", st.CurrentIndex, 2},
		{"ActiveProxy", st.ActiveProxy, "10.0.0.1:8080"}, // index 2 % 2 = 0
		{"ActiveCountry", st.ActiveCountry, "Indonesia"},
		{"ActiveCountryCode", st.ActiveCountryCode, "ID"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	if st.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %v, want >= 0", st.UptimeSeconds)
	}
}

func TestProxyAddr(t *testing.T) {
	tests := []struct {
		name  string
		proxy Proxy
		want  string
	}{
		{"ipv4", Proxy{IP: "1.2.3.4", Port: 8080}, "1.2.3.4:8080"},
		{"ipv6", Proxy{IP: "::1", Port: 8888}, "[::1]:8888"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.proxy.Addr(); got != tt.want {
				t.Errorf("Addr() = %q, want %q", got, tt.want)
			}
		})
	}
}

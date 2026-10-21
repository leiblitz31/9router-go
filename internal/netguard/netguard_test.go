package netguard

import (
	"net"
	"testing"
)

// TestIsPrivateOrReserved pins the ranges that make the guard worth having.
// The four at the end are the ones a hand-written predicate lost: each is a
// real internal service, not a hypothetical, and each was measured reachable
// through the copy of this check that lived in internal/proxy.
func TestIsPrivateOrReserved(t *testing.T) {
	blocked := []string{
		"127.0.0.1",       // loopback
		"10.1.2.3",        // RFC1918
		"172.16.0.1",      // RFC1918
		"192.168.1.1",     // RFC1918
		"169.254.169.254", // AWS/GCP metadata
		"0.0.0.0",         // unspecified
		"224.0.0.1",       // multicast
		"100.100.100.200", // Alibaba Cloud metadata (inside RFC6598 CGNAT)
		"100.64.0.1",      // RFC6598 CGNAT
		"192.0.0.192",     // Oracle Cloud metadata
		"198.18.0.1",      // RFC2544 benchmark
		"::1",             // IPv6 loopback
		"fe80::1",         // IPv6 link-local
		"2001:db8::1",     // documentation
	}
	for _, s := range blocked {
		if ip := net.ParseIP(s); ip == nil {
			t.Fatalf("bad fixture %q", s)
		} else if !IsPrivateOrReserved(ip) {
			t.Errorf("%s must be blocked", s)
		}
	}

	allowed := []string{
		"1.1.1.1",
		"8.8.8.8",
		"93.184.216.34",  // example.com
		"100.63.255.255", // just below the CGNAT block
		"100.128.0.1",    // just above it
		"2606:4700::1111",
	}
	for _, s := range allowed {
		if ip := net.ParseIP(s); ip == nil {
			t.Fatalf("bad fixture %q", s)
		} else if IsPrivateOrReserved(ip) {
			t.Errorf("%s must be reachable, the guard blocked it", s)
		}
	}

	if IsPrivateOrReserved(nil) {
		t.Error("a nil address is not a blocked address")
	}
}

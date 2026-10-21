package proxy

import "testing"

// DoHEndpoint's mapping itself is graded by TestDoHEndpoint in
// doh_transport_test.go. What is new with the dashboard page is the
// exported alias table behind GET /api/doh/defaults: it must carry every
// alias the resolver knows, and it must be a copy — a caller annotating
// the served map must not rewrite the table the data plane dials from.
func TestDoHAliasesReturnsCopy(t *testing.T) {
	aliases := DoHAliases()
	if len(aliases) != len(dohAliases) {
		t.Fatalf("DoHAliases() has %d entries, want %d", len(aliases), len(dohAliases))
	}
	for alias, endpoint := range dohAliases {
		if aliases[alias] != endpoint {
			t.Errorf("DoHAliases()[%q] = %q, want %q", alias, aliases[alias], endpoint)
		}
		if got := DoHEndpoint(alias); got != endpoint {
			t.Errorf("DoHEndpoint(%q) = %q, want %q", alias, got, endpoint)
		}
	}

	aliases["cloudflare"] = "https://example.invalid/dns-query"
	if got := DoHEndpoint("cloudflare"); got != "https://cloudflare-dns.com/dns-query" {
		t.Errorf("mutating the returned map changed the resolver's table: DoHEndpoint = %q", got)
	}
}

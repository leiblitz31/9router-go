package db

import (
	json "encoding/json/v2"
	"fmt"
)

// DefaultDoHPoolName is the name of the DoH resolver pool planted on first
// boot when an install has no doh pool yet. The name matches the pool the
// dashboard examples and tests use, so a seeded install and a hand-made one
// are indistinguishable in the UI.
const DefaultDoHPoolName = "doh-resolvers"

// DefaultDoHResolvers is the resolver list planted with the default DoH
// pool: the aliases and DoH JSON endpoints verified reachable from the
// reference install. Order is the pool's rotation order — the built-in
// aliases first, then endpoint URLs grouped by operator.
var DefaultDoHResolvers = []string{
	"cloudflare",
	"google",
	"https://one.one.one.one/dns-query",
	"https://security.cloudflare-dns.com/dns-query",
	"https://family.cloudflare-dns.com/dns-query",
	"https://mozilla.cloudflare-dns.com/dns-query",
	"https://doh.dns.sb/dns-query",
	"https://doh.pub/dns-query",
	"https://dns.alidns.com/resolve",
	"https://dns.adguard-dns.com/resolve",
	"https://dns.nextdns.io/dns-query",
	"https://basic.rethinkdns.com/dns-query",
	"https://doh.tiar.app/dns-query",
	"https://sm2.doh.pub/dns-query",
	"https://sky.rethinkdns.com/dns-query",
}

// SeedDefaultDoHPool plants the default DoH resolver pool when the install
// has no pool of type "doh" at all, and reports whether it created one.
//
// The seed is deliberately conservative: the existence of any doh pool —
// even an inactive or renamed one — means the operator has made their own
// arrangement, and the seed leaves the table exactly as it found it. It
// never pins the new pool to a provider; whether traffic uses it stays a
// dashboard decision. The insert goes through InsertProxyPool, so the row
// is byte-identical to one the dashboard's create form writes.
func (r *Repo) SeedDefaultDoHPool() (bool, error) {
	exists, err := r.hasDoHPool()
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if _, err := r.InsertProxyPool(ProxyPoolData{
		Name:        DefaultDoHPoolName,
		URLs:        append([]string(nil), DefaultDoHResolvers...),
		Type:        "doh",
		StrictProxy: true,
	}); err != nil {
		return false, fmt.Errorf("seed default doh pool: %w", err)
	}
	return true, nil
}

// hasDoHPool reports whether any row in proxyPools carries type "doh". The
// type lives inside the data payload, so rows are decoded rather than
// filtered in SQL — the table is small and this runs once per boot.
func (r *Repo) hasDoHPool() (bool, error) {
	rows, err := r.db.Query(`SELECT data FROM proxyPools`)
	if err != nil {
		return false, fmt.Errorf("list proxy pools: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return false, fmt.Errorf("scan proxy pool: %w", err)
		}
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue // an undecodable row is not a doh pool; InsertProxyPool owns row validity
		}
		if payload.Type == "doh" {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate proxy pools: %w", err)
	}
	return false, nil
}

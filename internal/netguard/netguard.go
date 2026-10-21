// Package netguard classifies addresses that must never be reached by an
// outbound request the gateway did not explicitly choose.
//
// The predicate lives in its own package so that every layer which has to
// answer "is this address internal?" can answer it the same way, and does so
// without an import cycle: internal/proxy already imports internal/translator,
// so the two cannot share a copy that lives in either one.
//
// One predicate, one place to fix it. The copy that lived in internal/proxy
// checked only net.IP's stdlib predicates and so missed 100.64.0.0/10, which
// puts Alibaba Cloud's metadata endpoint (100.100.100.200) inside a range the
// guard called public. The ranges net.IP's own methods do not cover are listed
// here; the stdlib predicates answer the rest.
package netguard

import "net"

// cidrBlocks are the ranges net.IP's own methods do not cover. They are
// separate from the built-in predicates because Go's IsPrivate() answers only
// RFC1918, IsLoopback only 127/8 and ::1, and neither knows about the
// addresses clouds and carriers actually use for internal services.
var cidrBlocks = func() []*net.IPNet {
	specs := []string{
		"100.64.0.0/10",   // RFC6598 carrier-grade NAT; 100.100.100.200 is Alibaba's metadata service
		"192.0.0.0/24",    // RFC6890 IETF protocol assignments, incl. Oracle's 192.0.0.192 metadata
		"198.18.0.0/15",   // RFC2544 benchmark range, used by some VM host-internal networks
		"192.88.99.0/24",  // deprecated 6to4 relay anycast
		"198.51.100.0/24", // RFC5737 documentation ranges: never a real image host
		"203.0.113.0/24",
		"::/128",         // unspecified
		"64:ff9b:1::/48", // RFC8215 local-use NAT64
		"2001:db8::/32",  // documentation
	}
	blocks := make([]*net.IPNet, 0, len(specs))
	for _, spec := range specs {
		if _, block, err := net.ParseCIDR(spec); err == nil {
			blocks = append(blocks, block)
		}
	}
	return blocks
}()

// IsPrivateOrReserved reports whether an address belongs to loopback, private,
// link-local, cloud-metadata, carrier-grade-NAT, documentation or benchmark
// space — any destination an outbound request has no business reaching unless
// the operator asked for it by name.
func IsPrivateOrReserved(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4.IsLoopback() || ip4.IsPrivate() || ip4.IsLinkLocalUnicast() ||
			ip4.IsLinkLocalMulticast() || ip4.IsUnspecified() || ip4.IsMulticast() ||
			ip4[0] == 0 {
			return true
		}
	} else if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	for _, block := range cidrBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

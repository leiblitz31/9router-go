---
type: fix
area: proxy
summary: Exempt the operator's proxy endpoints from the unified netguard dial guard
---

The netguard unification classifies 198.18.0.0/15 as reserved, which
includes the sandbox egress proxy (198.19.0.1). The ambient allowlist
only remembered the proxy's name as written, but the guard matches the
resolved dial address, so a hostname-named egress proxy was refused on
every dial — DoH resolver queries first. The ambient allowlist now also
records the resolved addresses, and a transport pinned to an assigned
proxy exempts that proxy (plus the ambient one) in its DialContext.

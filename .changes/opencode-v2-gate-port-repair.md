---
type: fix
area: proxy
summary: Port the v2 free-tier gate onto the local executor plumbing, refresh DoH fixtures
---

The #557 executor file could not be taken wholesale: it builds on the
declined #416 image helpers. Its three session-hint changes are applied
to the local ForwardOpencode and Go-free lanes instead. DoH test
fixtures move from TEST-NET-3 to public addresses, which the unified
netguard predicate now correctly treats as reserved.

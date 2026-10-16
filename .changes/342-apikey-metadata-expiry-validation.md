### Fixes
- **dashboard**: refuse an `expiresAt` that is not an RFC3339 timestamp. The auth middleware enforces an expiry with `time.Parse(time.RFC3339, …)` and skips a value it cannot parse, so a mistyped contract end was stored as a key that never expires; the refusal now names the expected shape (`2026-12-31T23:59:59Z`, or empty for no expiry).
- **dashboard**: validate the resale `metadata` blob instead of storing it verbatim. It must be a JSON object within 2 KB and may only carry `customerId`, `contact`, `notes`, `priceCents` (whole, non-negative cents) and `billingCycle` (`one-off`, `monthly`, `quarterly`, `yearly`); the refusal lists the accepted keys, because a typo such as `customerID` would otherwise drop that key out of the resale report silently.

### Changed
- **dashboard**: `PUT /api/keys/{id}` validates every field before the first write, so a refused policy no longer half-applies the fields that happened to be checked first — rate limits used to be committed before a bad expiry or an over-long name was rejected. The policy dialog's metadata help text now names the accepted keys and the size cap.

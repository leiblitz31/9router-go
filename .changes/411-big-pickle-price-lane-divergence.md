### Pricing and lane disagreements are recorded, not silent

- `pricing.KnownFreeModels` answers "what does it cost" while
  `providers.openCodeFreeTierModels` answers "which gate serves it". Two ids answer
  the two questions differently: `big-pickle` is billed at zero and routed on the
  keyed paid `/zen/v1` lane (#410), and `deepseek-v4-flash-free` is the mirror —
  routed to the keyless `/inference/v1` gate while the price table still resolves a
  paid row (0.14/0.28 per 1M, via the `deepseek-v*` glob rather than a row naming
  the id). Both are deliberate enough to leave alone, so this change alters no
  behaviour; it makes each disagreement explicit and guards it.
- `internal/providers/model_formats_pricing_divergence_test.go` adds the missing
  guard. `openCodePricingDivergences` stores **which way** each id disagrees
  (`pricedFree`/`lanedFree`) plus the reason, so an entry cannot silently describe
  a different disagreement than the one in the tree.
  `TestZeroPricedIdWithoutFreeLaneIsDocumented` covers the price→lane direction,
  `TestFreeLaneIdWithoutZeroPriceIsDocumented` the lane→price direction, and
  `TestRecordedDivergencesStillHold` fails when an entry stops describing a real
  disagreement or starts describing a different one.
- The `-free` suffix is no longer an unconditional exemption from the price check.
  It was justified by "the price table carries no row for it", which holds for nine
  of the ten ids in the lane list and fails for the tenth. The exemption is now the
  narrow claim it describes — no row means no cost — so a `-free` id that resolves
  to a paid row reaches the record instead of skipping the check.
- Both lists now state which question they answer in their doc comments, and the
  "cannot drift" claim that was false for an id already in the tree is gone.
- Verified with `go build ./...`, `go vet` on both packages, and the full
  `go test ./...`. The guard was proven to bite rather than assumed to. Six
  mutations, each restored afterwards:

  ```
  A  new -free id that hits the deepseek-v* glob (deepseek-v4-pro-free) → FAIL
     "routed to the free gate but not billed at zero; record the reason"
     (the same mutation passes on the previous commit — the hole was real)
  B  the deepseek-v4-flash-free entry removed                         → FAIL same message
  C  a recorded reason emptied                                        → FAIL "without a reason"
  D  the id added to KnownFreeModels so both lists agree               → FAIL "now agree; drop the entry"
  E  the recorded direction flipped                                    → FAIL "no longer describes this id"
  ```

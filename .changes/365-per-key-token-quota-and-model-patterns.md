### feat(access): allowlist globs and deny rules for per-key model access

Per-key model allowlists gain the pattern vocabulary issue #365 asks for, ported
from decolua/9router `keyAccess.js` (v0.5.104): a wildcard that spans the `/`
(`*claude*`), deny entries (`!opus`, `-haiku`), and a deny-only list that opens
everything it does not name.

**Why:** the allowlist matched through the resolver's own glob
(`providers.MatchModelPattern`, backed by `path.Match`), where `*` is
segment-local. An operator writing `*claude*` — the spelling the issue and
upstream both use — got a key that denied every model, and there was no way to
express "everything except X" short of enumerating the catalogue. Measured
before the change: 7 of 17 cases in `TestModelAllowedPatterns` failed, among
them `*claude*` against `anthropic/claude-sonnet-4` and every deny-only list.

**What changed:**

- `providers.MatchAllowPattern` — the allowlist glob. `*` spans `/`, matching is
  case-insensitive and anchored at both ends, the pattern is literal except for
  `*`, and a pattern without `*` still answers exactly as before.
  `MatchModelPattern` is untouched so the resolver's capability patterns keep
  their segment-local semantics; a test pins that the two cannot be collapsed
  into one by accident.
- `providers.IsDenyEntry` / `providers.MatchDenyEntry` — `!term` / `-term`
  parsing (a lone sigil is a model id, not a rule) and deny matching, which
  accepts a glob or a substring so `-haiku` needs no asterisk.
- `chat.modelAllowed` — parses the list into positives and denies, checks denies
  first so a deny always wins over a positive rule, and reads a list with no
  positive entry as deny-only: open except denied. An empty or blank-only list
  still means "no allowlist configured" and allows everything, which is what
  keeps every key minted before this feature behaving as it did.
- Dispatch and listing still share the one decision function, so a denied model
  is hidden from `/v1/models` as well as refused at `/v1/chat/completions`.

**Verified:** `go test ./internal/providers/ ./internal/handlers/chat/` — both
packages ok; the 7 previously failing cases are green alongside 40 new matcher
cases.

### feat(keys): meter per-key token quotas from the caller, and let a budget be reset

Per-key token budgets are made real (#365): the spend a `tokenQuota` is measured
against is now attributed to the client that spent it, an exhausted key can be
given a fresh budget without deleting its history, and the dashboard can read the
spend it is rendering.

**Why — the quota was inert.** `usageHistory.apiKey` holds the **upstream**
credential (masked), not the caller: the usage writer stores
`maskAPIKey(info.APIKey)`, and the suite pinned that behaviour elsewhere. The sum
a quota was measured from therefore never matched a client key. On the live
database this was 0 rows out of 634, so `checkApiKeyQuota` could never fire and a
runaway client was never bounded — which is the first thing the issue asks for.
The unit test that appeared to cover this inserted its own rows keyed by the
client key, so it passed without touching the real write path.

**What changed:**

- `usageHistory.clientKey` (additive column) records the caller, masked, written
  from the request context in `logUsage` through the new
  `middleware.AuthenticatedApiKeyFromContext` — the auth middleware already put
  the key on the context, so no request-path change was needed.
  `InsertUsageHistory` takes it as a trailing variadic so the fixtures and
  migrations that have no caller to name keep compiling unchanged.
- `tokenUsageSince` sums `clientKey = ? OR apiKey = ? OR apiKey = ?`: the last
  two keep rows written by the upstream Next.js dashboard counting, since that
  ledger stored the client key where this one stores the upstream credential.
  No index is added — the predicate is an OR across two columns, which no
  single-column index serves.
- `apiKeys.quotaResetId` plus `ResetApiKeyQuota` and
  `POST /api/keys/{id}/reset-quota`: the mark is the `usageHistory` row id the
  budget is measured from, taken in the same transaction as the write. A row id
  and not a timestamp because `usageHistory.timestamp` is RFC3339 at second
  precision, so a reset issued in the same second as the spend that exhausted
  the key would compare equal and keep charging it — exactly the case an
  operator hits when they reset a key they just watched fail.
- `GET /api/keys` carries `usedTokens` next to `tokenQuota`, so the dashboard can
  render a budget instead of guessing whether a key is untouched or exhausted.
- The keys table shows the pair as `spent / budget`, paints an exhausted key in
  the danger tone, and gains **Reset token quota** on a metered key's action menu
  — the reset runs without a confirmation because it is the recoverable
  direction, and the usage history is never touched by it.

**Verified:** `go test ./internal/...` — 43 packages ok; the integration suite ok,
including a new end-to-end test that spends past a one-token budget, is refused
with 403 and the `X-Token-Quota-*` headers without reaching the provider, is reset
through the endpoint, and is then served again. A new db test pins that an
unattributed row (upstream credential only) is not charged to any client key,
while a legacy row carrying the client key still is.

**Divergence from upstream, on purpose:** the refusal stays **403** with a
`Token quota exhausted` body, not the issue's 429 `key_quota_exceeded`. The port
documents why: a lifetime budget is not a per-window rate, so `Retry-After` and
`X-RateLimit-Reset` would both promise that waiting helps when only an operator
raising the quota does.

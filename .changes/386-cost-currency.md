### Cost display in any currency (USD, IDR, EUR, …)

- **What changed.** Settings gains a **Currency** section: pick the currency every cost is shown
  in, edit the rate table (one USD in each of thirteen currencies), turn dual display on to see
  the USD figure next to the converted one, and refresh the rates from a public provider in one
  click. Costs themselves are still measured and stored in USD
  (`internal/pricing` → `usageHistory.cost`); this is a display layer over them.
- **Every cost surface follows the choice.** Usage overview KPI, the breakdown table's Costs
  view, per-request cost in the request details, the trend-chart tooltip, and the cache and
  compression savings figures now format through one helper
  (`web/src/lib/currency.ts`, `costIn()` in `currency.svelte.ts`), so a change reaches the
  analytics without a reload. The compression CSV keeps its raw `EstUSD` column and appends
  `EstLocal` plus `Currency`, so an export stays machine-readable.
- **Locale-aware amounts.** `Intl.NumberFormat` writes each amount the way its market does —
  `Rp 18.014` groups with dots and carries no decimals, `$0.0123` keeps the four digits the
  request details always showed. With USD selected the output is byte-identical to before, so
  the feature is opt-in.
- **Storage.** `settings.displayCurrency` = `{ code, rates, showBoth, updatedAt }` through the
  existing `GET/PUT /api/settings`, so it survives a reload and travels with the database
  export. `internal/handlers/dashboard/currency.go` normalises the payload — three-letter
  codes only, positive finite rates, capped at 24 entries, `updatedAt` only when it parses —
  while `web/src/lib/currency.ts` owns the catalog, the seed rates and the format rules.
- **Refresh without losing the manual table.** `POST /api/settings/currency/refresh` reads
  `{code, showBoth, codes}` and fetches only the codes the catalog asked for, trying
  `open.er-api.com` and then `api.frankfurter.app` (both key-less) with an 8s timeout each. When
  every provider is unreachable it answers 502 and the stored rates are left untouched: it is a
  refresh, never a clear.
- **Verified.** `go test ./internal/handlers/dashboard/ -run Currency` (normaliser table,
  PUT/GET round-trip, refresh with a dead primary provider, 502 keeps the stored table, missing
  codes → 400), `bun test src/lib/currency.test.ts`, `bun run build`, the svelte-check ratchet,
  and a browser run on an isolated `DATA_DIR`: select IDR + dual display + a manual rate →
  saved as `{code:"IDR",rates:{IDR:16500},showBoth:true}`; the Usage KPI then renders
  `~Rp 0 ($0.00)`; the panel preview reads `1 USD = Rp 18.014`; **Refresh from provider** pulled
  live rates (IDR 18014.03, EUR 0.8896, JPY 158.00, SGD 1.2799) and stamped `updatedAt`.

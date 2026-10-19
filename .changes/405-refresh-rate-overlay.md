### Keep manual currency rates across a partial refresh

A rate provider publishes only the subset of currencies it carries — the primary
provider (`open.er-api.com`) carries VND, while the fallback (`api.frankfurter.app`)
does not. `HandleRefreshCurrencyRates` previously replaced the whole stored
`displayCurrency.rates` table with the fetched map, so a refresh answered by the
fallback silently deleted any manual rate an operator had typed for an
uncovered currency and reset the cost surfaces to the compiled seed.

- `internal/handlers/dashboard/currency.go`:
  - `mergeRefreshedRates` overlays the fetched rates on top of the stored table,
    so every pre-existing manual rate survives while the provider's fresh rates
    overwrite matching codes. Stored codes hold their `maxCurrencyRates` slots:
    a table already at the cap cannot lose its alphabetically-last manual rate to
    a code the provider published, and a fetched rate that does not fit is simply
    not stored (it shows up as a lower `ratesStored`, not as a deleted rate).
  - Returns `ratesMissing`, reporting requested codes absent from the provider's
    payload in request order. Both the primary response path and the fallback
    response path include `ratesMissing: missing`.
  - `ratesStored` now explicitly counts the provider rates that were actually
    stored into the table rather than the table's total size, disambiguating
    refreshed rates from kept manual rates.
  - When the POST payload omits `showBoth`, the handler now inherits the
    currently stored `showBoth` flag (falling back to `false` when no setting
    row exists) instead of hardcoding `true`. This matches the PUT normalizer's
    omission default and stops a refresh from flipping dual-display on as a side
    effect.
- `web/src/api/client.ts`:
  - Extended `refreshCurrencyRates` return type with `ratesMissing: string[]`.
- `web/src/lib/currency.ts` & `ProfileSettingsView.svelte`:
  - Added pure `describeRefreshRates(stored, missing, host)` helper and tests in
    `web/src/lib/currency.test.ts`.
  - The Settings refresh banner informs operators which currencies were kept at
    their manual rate (e.g. `Refreshed 1 rates from api.frankfurter.app. VND kept at your manual rate.`).
- Tests:
  - Table-driven tests in `TestMergeRefreshedRates` verifying manual retention,
    fetched overwrites, request-order missing lists, unparsable row resilience,
    and cap enforcement.
  - Handler regression tests `TestHandleRefreshCurrencyRatesKeepsManualRates`
    and `TestHandleRefreshCurrencyRatesShowBoth`.

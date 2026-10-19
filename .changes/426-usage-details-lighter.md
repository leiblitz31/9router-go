### Usage Details got lighter, and two Overview panels moved to the bottom

The analytics Overview no longer puts its two optional panels in the middle of
the page, and the Details dashboard stopped scanning the whole usage ledger on
every load.

**Combo flow and Usage trend** now sit at the very bottom of the Overview, below
the usage-by-model breakdown, both collapsed by default with their header and
chevron still visible. Two new switches in **Settings > Display** — "Combo flow"
and "Usage trend" — remove either panel from the Overview entirely. Visibility and
the collapsed state are separate on purpose: the switch decides whether the panel
exists, the chevron whether it is open.

**Details: the heaviest query is gone.** `GET /api/usage/dashboard` used to run
`SELECT promptTokens, tokens FROM usageHistory WHERE tokens LIKE '%cache%'` — a
substring `LIKE` no index can serve, so each load scanned the entire ledger and
JSON-decoded every matching row. Measured on a 150,000-row ledger it took 67 ms
against 64/39/31/9/1/0.1 ms for the six other queries the same handler issues, and
it was the only one whose cost grew with the whole table instead of a window. The
dashboard endpoint no longer runs it.

The Cache Hit Rate card stays, computed from the trailing-7-day history the
handler already reads for Most Used Model, so it adds no query. **Its number is
now a 7-day window rather than a lifetime figure**, and the card caption says
exactly that. The `cacheTokens` and `cacheRequests` response fields, which
nothing read, are gone.

`ComboFlowCard`'s Indonesian UI text — counters, handoff notes, legend and the
latency tooltip — is now English, matching the rest of the dashboard.
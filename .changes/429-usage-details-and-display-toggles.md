### Usage Details cards cleanup and Settings display toggle reactivity

- Removed redundant top summary KPI cards in the **Usage > Details** tab (Total Requests, Lifetime Tokens, Peak Day Tokens, Cache Hit Rate, and Most Used Model) as they are already available in **Usage > Overview**.
- Enhanced **Settings > Display** toggles for **Combo flow** and **Usage trend**:
  - Turning off Combo flow now completely skips `GET /api/combos` network requests and prevents keeping combo definitions in memory.
  - Setting changes immediately react across the dashboard via custom event dispatching and `$state` synchronization.
  - Clarified toggle descriptions in Settings > Display.
- Verified with `bun test`, `make vet-svelte`, and `make build`.

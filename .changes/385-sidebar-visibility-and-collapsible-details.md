### Sidebar visibility (Plans) and collapsible usage details

- **One switch, one entry.** The **Display** card carries the only sidebar switch: **Advanced usage**
  (the `plans` tab). It is the only entry the panel offers, so a deployment that never opens that
  page does not carry it around. The switch is a saved setting, so the floating "Unsaved changes —
  Save" reminder covers it.
- **What turning it off does.** The sidebar entry (labelled **Plans**) leaves the sidebar, its URL
  stops resolving, and the **Plan** field in the API-key editor disappears with it — the field points
  at the same feature, so it would otherwise link to a page that redirects. The note under the switch
  says exactly that.
- **Defaults.** A deployment that never saved a list starts with Plans hidden; Security is visible
  like every other entry. Saving an explicit empty list restores everything — only an absent or
  unreadable stored value falls back to the default.
- **Usage detail panels collapse.** In Usage > Details, **Activity Heatmap** and **Node Health**
  start collapsed and open from the chevron in their card header. The starting state is the new
  **Display > Expand usage details** switch (localStorage `detailsExpandedByDefault`, off by
  default), rendered next to Unit abbreviation and applied to both panels.
- **Storage.** `settings.navVisibility.hidden` (a list of entry ids) through the existing
  `GET/PUT /api/settings`, so it survives a reload, travels with the database export/import, and
  applies to any browser. `internal/handlers/dashboard/navvisibility.go` normalises the payload —
  lower-cased, de-duplicated, capped at 32 ids, `settings` stripped — while `web/src/lib/nav.ts`
  owns the canonical entry list and the toggleable set. `web/src/lib/navVisibility.svelte.ts` mirrors
  the resolved list for consumers outside the settings flow, so the key editor reads it instead of
  having the flag threaded through three components.
- **Deep links.** A hidden tab is not reachable by its own URL: `App.svelte` redirects to
  Endpoint & Key, alias paths included (`/dashboard/keys`, `/dashboard/terminal`,
  `/dashboard/usage/cache`), and a hidden Media Providers accordion hides every child. The guard
  waits for the boot settings fetch: before it resolves the store is empty, and the fallback list
  would bounce a deep link to an entry the operator has since made visible.
- **One entry list.** The sidebar and the panel render entries from `web/src/lib/nav.ts` instead
  of three hand-written arrays plus a hardcoded Plans link, so a switch cannot address an id the
  sidebar does not consume (the phantom-config-path failure mode).
- **Verified.** `go test -count=1 ./internal/handlers/dashboard/`, `bun test src/lib/nav.test.ts
  src/lib/displayPrefs.test.ts` (22 pass), `bun run build`, the svelte-check ratchet (81 errors =
  baseline, 0 unresolved identifiers), and a browser run against an isolated `DATA_DIR`: a fresh
  database hides Plans while Security stays; the Display card holds the three switches (Unit
  abbreviation, Expand usage details, Advanced usage) with no separate Sidebar card left; the
  sidebar entry reads **Plans** while the switch row reads **Advanced usage**; on Usage > Details
  both cards report `aria-expanded="false"` with no body until the Display switch opens them;
  `/dashboard/plans` renders on a load and a reload once visible and redirects to
  `/dashboard/endpoint` while hidden; and the key editor's **Plan** select (headings `Name, Plan,
  Rate limits, …`) is present with the feature on and gone with it off (`Name, Rate limits, …`).

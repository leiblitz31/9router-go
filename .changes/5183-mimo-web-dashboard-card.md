### fix(dashboard): give mimo-web its provider card

The mimo-web port (upstream decolua/9router#5183) registered the executor,
the provider definition and the model catalog, but never added the frontend
catalog entry, so the dashboard rendered no card for the provider: there was
no surface to add, rotate or replace a Studio session, and the only way to
create a connection was writing `providerConnections` by hand.

- `web/src/lib/providers.ts` gains the `mimo-web` entry beside `xiaomi-mimo`:
  category `webCookie`, `authType: cookie`, alias `mw`, and an auth hint that
  names the three cookies the credential must carry (`serviceToken`, `userId`,
  `xiaomichatbot_ph`) and the serviceToken's roughly one-day lifetime.
- `web/public/providers/mimo-web.png` ships the Xiaomi MiMo artwork (the same
  brand mark as `xiaomi-mimo.png`), satisfying the catalog artwork contract.

Verified: `bun test src/lib/providers.test.ts` 15/15 (the artwork test failed
on the catalog entry alone and passes with the asset), `bun run build`, and a
live probe of `mimo-web/mimo-v2.6-flash` / `mimo-v2.6-pro` against a
connection created through the new card's credential shape.

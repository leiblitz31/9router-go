### Fixed

- Cloudflare Workers AI (`cf/*`) chat requests no longer 404 on deployments
  without a `CLOUDFLARE_ACCOUNT_ID` env var. The registry baked that variable
  into the provider BaseURL at startup, so an unset variable produced
  `/client/v4/accounts//ai/v1/chat/completions` and Cloudflare answered 7003
  for every model, while the dashboard connection test — which substitutes
  the connection's own account id — stayed green. The registry now keeps an
  `{accountId}` placeholder when the env var is unset (the env var still wins
  for single-account deployments), and `getProviderConfig` fills the
  placeholder from the selected connection's
  `providerSpecificData.accountId`, the same field the dashboard test reads.
  `cloudflareAccountFromBaseURL` treats the placeholder as "no account id"
  instead of extracting it literally.
- Verified by unit tests covering the registry URL builder, placeholder
  substitution in `getProviderConfig` (direct and via a seeded connection
  row), and the unchanged "Missing Account ID" dashboard validation outcome.

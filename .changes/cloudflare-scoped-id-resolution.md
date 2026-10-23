### Fixed

- Model ids in Cloudflare's native scoped form now resolve. A client that
  copies a Workers AI id from Cloudflare — `@cf/moonshotai/kimi-k2.7-code`,
  with no `cf/` router prefix — had the id split at the first slash, so the
  "provider" became `@cf` and the request failed with
  `no active connections for provider: @cf` (HTTP 502), while the prefixed
  form `cf/@cf/...` worked. `resolveModel` now recognises the `@cf/` scope —
  Cloudflare's own provider namespace, part of the upstream id itself — and
  resolves it to provider `cloudflare-ai` with the model id, scope included,
  untouched.

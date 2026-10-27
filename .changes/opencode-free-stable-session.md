### Opencode free lane ignores client sessions, uses the stable session

- `opencodeSessionHint` on the free lane now always returns the router's
  stable per-connection session, even when the client sent its own session
  id. Upstream answers client-supplied sessions on this tier with 429
  FreeUsageLimitError while the stable session passes (measured Oct 2026:
  agent-originated requests carrying their own canonical session failed
  three times in a row; probes without one passed in the same minutes).
- `opencodeStableSessionKey` on the free lane now returns the stable key
  even for requests with a client session, so session rotation stays armed
  there. Keyed lanes are untouched: the client's session still wins and is
  never rotated.

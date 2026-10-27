### Opencode free-lane request debug log (diagnostic)

- `ForwardOpencode` chat path logs one `ocdbg` line per attempt: model,
  connection, client session hint, resolved upstream session, target URL,
  relay flag, SHA-256 of the outbound body, body length, client stream flag,
  upstream status. Body content is never logged, only its hash.
- Purpose: diff agent-originated requests (429 free-tier refusals, Oct 2026)
  against passing probes without changing request handling.

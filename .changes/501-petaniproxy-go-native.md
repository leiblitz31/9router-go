### Petani-Proxy Go-Native: Gateway Manager In-Process

- Ported all 13 petani-proxy Python modules to Go in `internal/petaniproxy/`
  (fetcher, checker, validator, server, warp, scheduler, exporter, updater,
  bridge, webshare, grokfarm, zaifarm, tui) — 36 files, 215/215 tests pass.
- `internal/proxygateway/manager.go` rewritten: gateway now runs in-process
  via `petaniproxy.NewServer()` on 127.0.0.1:8888 with background harvester
  (fetch → validate → update pool every 10 min). No more Python subprocess.
- Fixed 4 Python bugs during porting: silent warp sync failure (raw db_path),
  IPv6 crash in `endpoint.split(":")`, HTTP 500 accepted as valid IP in
  `fetchHostIP`, nil-pointer race in bridge `serve()`.
- Farm modules (webshare/grok/zai) expose interfaces for browser, captcha STT,
  and IMAP — production wiring (chromedp, Whisper API, go-imap) pending.

Verified: `go test ./internal/petaniproxy/ ./internal/proxygateway/` all green,
`go vet` clean, `go build ./...` OK, E2E `TestE2EGatewayRotation` passes.

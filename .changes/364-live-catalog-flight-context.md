### fix(models): stop one client's disconnect cancelling the shared catalog fetch

`resolveLiveCatalog` collapses racing requests into one upstream call via
`singleflight`, but the closure captured the leader's request context, so the
leader's client hanging up cancelled the shared fetch for every waiter. An empty
result caches nothing — `liveCatalogStoreModels` is only reached when
`len(discovered) != 0` — so the next dashboard poll repeated the whole call and
the cache never filled: on a slow node that is a permanently cold cache plus a
fan-out to the provider on every refresh.

The production log showed the shape over 1.5h of dashboard traffic: 58
`compatible node catalog fetch failed ... context canceled` against only 10
`context deadline exceeded`, concentrated on the three slowest nodes. Cancels
outnumbering real timeouts nearly 6:1 means the *client* went away — and took
everyone waiting on the same flight with it.

`context.WithoutCancel` detaches the shared call from whoever started it. Each
fetch's own `liveCatalogFastHTTP` timeout remains the only bound, so the request
cannot outlive the 10s ceiling that was already there.

**Verified.** `live_catalog_flight_test.go` drives the real race: the upstream
blocks until released, the leader's context is cancelled mid-flight, and a
follower joins — it must receive the shared result with the upstream called
exactly once. The upstream answers only its first request, so the assertion
cannot pass by luck. Run against the pre-fix code it reproduces the production
signature exactly (`compatible node catalog fetch failed ... context canceled`,
then `follower got []`). `gofmt -l`, `go vet ./...`, `go build ./...` and
`go test ./...` all clean.

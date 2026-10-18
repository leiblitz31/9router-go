### test(media): gate the live systemone test like the other live tests

`TestHandleSystemone_Live_Opencode` makes a real request to
`oc/jev-1.13-free`, but it only checked `testing.Short()`, which CI does not set,
so it ran on every build. That free tier now answers `429 FreeUsageLimitError`
for callers outside its own client, so the test fails on `main` and on every
branch cut from it while saying nothing about the code under test: on `main` at
`e880043` it is what turns both `test` and `Race detector` red.

Its neighbours in the same package (`e2e_media_test.go`) gate live network tests
on `ROUTER_LIVE_E2E`; this one now does the same and keeps the `testing.Short()`
guard. Enabling it explicitly still surfaces the 429 rather than hiding it — that
is what a live test is for; it just does not run by default any more.

**Verified.** `go test ./internal/handlers/media/` passes with the live test
skipping and the three mocked systemone tests still running;
`ROUTER_LIVE_E2E=1 go test -run TestHandleSystemone_Live_Opencode` still fails on
the provider's 429, unchanged.

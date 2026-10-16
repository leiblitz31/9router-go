### ci: stop a module-proxy hiccup from reddening the cross-compile job

The `cross` job added in #356 starts five `go build` invocations via `make cross`,
and Go fills the module cache as it goes, so one transient read error from
`proxy.golang.org` aborts whichever target hit it. That happened on the first
push run after the merge:

```
modernc.org/sqlite@v1.54.0: read
  "https://proxy.golang.org/modernc.org/sqlite/@v/v1.54.0.zip":
  stream error: stream ID 1; INTERNAL_ERROR; received from peer
```

The identical job passed on a re-run with no code change, which is what makes it
a false signal: a network hiccup read as "a release target is broken", and that
is precisely the failure this job exists to report. Every PR runs it, so the
flake would have reddened unrelated PRs.

`go mod download all` now runs first and is retried three times. It is the only
step in the job that touches the network, so the five builds that follow are
served from the cache. A failure on all three attempts still fails the job, with
a message that says explicitly it is infrastructure rather than the diff.

Verified: the retry loop was exercised against a simulated two-failure sequence
and succeeds on the third attempt; a cold `go mod download all` followed by
`make cross` builds all five targets. `actionlint` passes on all three workflows.

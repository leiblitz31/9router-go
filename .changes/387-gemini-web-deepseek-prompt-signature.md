### fix(gemini-web): pass transcript flag to deepSeekMessagesToPrompt

Fixes a compile break on `main` caused by a signature mismatch in `geminiWebMessagesToPrompt`. Commit `dca23163` (#352) added a boolean `transcript` argument to `deepSeekMessagesToPrompt`:

```go
func deepSeekMessagesToPrompt(messages []deepSeekMessage, historyWindow int, transcript bool) string
```

`geminiWebMessagesToPrompt` was still calling it with two arguments (`(messages, 0)`). Passing `false` preserves Gemini Web's self-contained turns where session-persistence transcript formatting is not needed.

**Verified:**
- `go build ./...` compiles clean with no errors.
- `go test ./internal/proxy/executor/...` passes.
- Full test suite `go test ./...` passes across all 46 packages.
- `make run` compiles and successfully serves HTTP 200 on port 20130.

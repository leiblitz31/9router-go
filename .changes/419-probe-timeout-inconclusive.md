### [417] - 2026-10-18

#### Changed
- Raised `testModelTimeout` from 15s to 45s in `internal/handlers/media/model_test_ping.go` to accommodate reasoning-heavy free models.
- Treated mid-stream timeouts / aborts as inconclusive probes: reporting `ok=true` if any SSE `data:` bytes arrived before timeout, and `ok=false` with an empty error string if no response was received.

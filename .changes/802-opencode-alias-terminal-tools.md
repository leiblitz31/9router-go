---
"9router-go": patch
---

fix(opencode): alias declared tool names containing "terminal" on the free tier

- The free tier answers 429 FreeUsageLimitError whenever tools[] declares a function whose name contains "terminal" (case-insensitive substring)
- ConcealFingerprintTools moves gated declarations to alias run_command (numbered on collision), retargets tool_choice and history tool_calls, and restores the caller's original tool names on response payloads
- Ported from upstream decolua/9router#5328 (fixing #5319)

### fix(capabilities): send reasoning_effort for OpenCode's glm-5.3-flash-free

Upstream decolua/9router#5003 parity. OpenCode's own inference service serves
`glm-5.3-flash-free` (not z.ai) and strictly validates the request body, so
the zai-shaped `thinking:{type:"enabled"}` from the global `*glm-5.3*` pattern
is rejected with 400 `unknown_parameter`. Added an exact-id override in the
`opencode-go` provider capabilities with `ThinkingFormat: "openai"` so the
gateway emits `reasoning_effort` instead. Other providers (z-ai, glm, glm-cn)
keep the zai shape. Verified with a table-driven unit test asserting the
opencode-go override resolves to `openai` while z-ai still resolves to `zai`.

### fix(models): deepseek-v4.1-flash-free inherits v4 family formats

Upstream decolua/9router#4998 parity (model-family half). The OpenCode family
fallback used prefix `deepseek-v4-flash` which does not match the `.1` version
segment in `deepseek-v4.1-flash-free`, so it fell through to chat-only instead
of inheriting `[openai, claude, openai-responses]`. Added `deepseek-v4.1-pro`
and `deepseek-v4.1-flash` prefixes mirroring the v4 rows. The proxy-pool
fallback half of #4998 targets the Node.js `src/sse/services/auth.js`
architecture (virtual connection tagging + `excludeConnectionIds`) which has
no direct equivalent in the Go auth layer; not ported.

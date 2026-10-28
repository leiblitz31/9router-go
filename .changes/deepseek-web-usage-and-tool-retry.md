# deepseek-web: estimated usage and tool-retry hint

- deepseek-web non-streaming and tool-turn answers now report estimated token
  usage instead of zeros: the site reports no counts, so prompt, answer, and
  reasoning texts are estimated with the same method this repository already
  uses for a web provider that reports no counts (four characters per token,
  with the CJK adjustment; see geminiWebEstimateTokens). The counts are
  estimates, not upstream measurements.
- Tool-calling prompts on this provider's path only now carry one retry rule:
  a tool result that reports an error or an unsupported parameter combination
  may be retried once with a different or reversed combination before the
  model reports failure. Repeating the identical call is still an error, and
  data the tool did not return must never be invented. The hint is appended
  at the deepseek-web prompt site only, so gemini-web's shared tool renderer
  is untouched.
- Companion to upstream luqman-v1/9router-go#732 (DSML scrub on no-tools
  paths, ported in `.changes/732-deepseek-dsml-scrub-nontool-paths.md`).
  Cold-start first-turn latency was investigated and deliberately left alone:
  token exchange is already cached, sessions are already pinned, and the
  stealth jitter default is 400ms, so no redundant first-turn round trip was
  found to remove; the single 35.4s first probe is consistent with upstream
  generation variance, not a systematic cold-start cost.

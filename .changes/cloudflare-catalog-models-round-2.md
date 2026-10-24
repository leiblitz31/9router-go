### Added

- Seven more Workers AI chat models from the official Cloudflare catalog
  are now seeded in the static registry for both the `cf` and
  `cloudflare-ai` provider ids, with matching entries in the dashboard
  catalog (`web/src/lib/models.ts`, `cloudflare-ai` block):
  `@cf/deepseek-ai/deepseek-v4-pro-0813`,
  `@cf/deepseek-ai/deepseek-v4-flash-0731`, `@cf/zai-org/glm-5.3`,
  `@cf/zai-org/glm-5.3-flash`, `@cf/openai/gpt-oss-20b`,
  `@cf/qwen/qwen3-30b-a3b-fp8`, `@cf/meta/llama-3.2-11b-vision-instruct`.
  The System One decision models `@cf/cloudflare/clef` and
  `@cf/cloudflare/clef-omni` landed separately in the preceding commit
  (`5db188f8`); their dashboard entries are added here.
- The registry re-seeds the seven chat models from the previous catalog
  round (`@cf/qwen/qwen3.8-27b`, `@cf/zai-org/glm-5.2`,
  `@cf/moonshotai/kimi-k2.7-code`, `@cf/openai/gpt-oss-120b`,
  `@cf/google/gemma-4-26b-a4b-it`, `@cf/meta/llama-4-scout-17b-16e-instruct`,
  `@cf/nvidia/nemotron-3-120b-a12b`): a parallel registry rebuild on the
  remote had dropped them from the static lists, and restoring them keeps
  the registry, the dashboard catalog and `/v1/models` in parity.
- Verified live before registration: gpt-oss-20b, llama-3.2-11b-vision,
  clef and clef-omni answer in ~1-2s; glm-5.3-flash in ~4s;
  deepseek-v4-flash-0731 in ~9s; qwen3-30b-a3b-fp8 answers but slowly
  (~122s for a short reply). glm-5.3 and deepseek-v4-pro-0813 did not
  answer within 110s (large flagship models on Workers AI are slow;
  registered under their official ids for catalog parity).

### Removed

- `deepseek/deepseek-v4-pro` from the `cf`/`cloudflare-ai` registry lists
  and the dashboard catalog. That id matches no official Workers AI model
  (the real one is `@cf/deepseek-ai/deepseek-v4-pro-0813`, now registered);
  it was added in the previous round for catalog parity and never answered.
  The `commandcode` provider's own `deepseek/deepseek-v4-pro` entry is a
  different provider's catalog and is untouched.

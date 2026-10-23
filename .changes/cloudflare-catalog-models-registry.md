### Added

- Eight Workers AI chat models that the cloud catalog already served through
  `/v1/models` are now also seeded in the static registry for both the `cf`
  and `cloudflare-ai` provider ids, so they appear in the dashboard's
  provider model lists without depending on catalog sync:
  `@cf/qwen/qwen3.8-27b`, `@cf/zai-org/glm-5.2`,
  `@cf/moonshotai/kimi-k2.7-code`, `@cf/openai/gpt-oss-120b`,
  `@cf/google/gemma-4-26b-a4b-it`, `@cf/meta/llama-4-scout-17b-16e-instruct`,
  `@cf/nvidia/nemotron-3-120b-a12b`, `deepseek/deepseek-v4-pro`.
  The same eight entries were added to the dashboard catalog
  (`web/src/lib/models.ts`, `cloudflare-ai` block) to satisfy
  `TestDashboardCatalogListsEveryRoutedModel`. All are chat models, so no
  model-format entries were needed (chat is the default kind). Verified live before registration: seven answer chat
  completions with HTTP 200 (glm-5.2 is slow, ~61s for a short reply);
  `deepseek/deepseek-v4-pro` is registered for catalog parity but did not
  answer within 120s in two attempts.

### Free lane moves to /inference/openai/v1, UA bumped to 2.1.4

- The keyless oc free lane (ForwardOpencode, chat and responses, including
  relay paths) now targets the current OpenAI-compatible gate prefix
  `/inference/openai/v1` instead of the legacy `/inference/v1`, which the
  gate answers with intermittent 429 FreeUsageLimitError
  (luqman-v1/9router-go#724; azrinaldi/opencode-free-bridge routes its chat
  and responses lanes there).
- New `opencodeFreeInferenceBase` helper maps both the zen layout and the
  legacy prefix onto openai/v1 and leaves an already-migrated base untouched;
  `opencodeChatURL` routes through it.
- Scope is strictly the oc lane: the keyed zen lane keeps
  `opencodeInferenceBase` (`/inference/v1`) and the opencode-go lane keeps
  its own mapping and relay paths, so their tests are untouched.
- `DefaultOpenCodeUA` bumped from `opencode/latest/2.0.29/cli` to
  `opencode/latest/2.1.4/cli`; the keyed lanes keep `DefaultOpenCodeV1UA`
  (`opencode/1.18.31`).
- Verified by new table tests for `opencodeFreeInferenceBase` and
  `opencodeChatURL`, updated relay/routing expectations in the oc-lane
  tests, and a live probe of `oc/deepseek-v4.1-flash-free` through the
  deployed router after this change.

### Fixes

- **media**: forward `providerCfg.StaticHeaders` on outbound `/v1/embeddings` requests, ensuring GitHub Copilot embeddings receive required headers (`copilot-integration-id`, `editor-version`, `x-github-api-version`) (closes #398).

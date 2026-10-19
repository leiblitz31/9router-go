### Fixes

- **db**: encrypt nested `providerSpecificData` credentials (console API keys, session tokens, usage scraping cookies) at rest in the Vault envelope, ensuring full confidentiality on disk and transparent hydration on read (closes #413).

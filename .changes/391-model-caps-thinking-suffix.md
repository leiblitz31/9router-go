### Fixes

- **dashboard**: strip thinking level suffixes (such as `(high)` or `(medium)`) in `isModelFree` before checking pricing tables and free-tier heuristics, ensuring models like `nemotron-3-ultra-free(high)` correctly receive the `FREE` badge on provider cards (closes #390).

### Fixes

- **translator**: sanitize nested array schemas, tuple items, and empty `{}` item definitions when converting schemas for Gemini and Google Cloud Code, preventing upstream 400 `INVALID_ARGUMENT: items.items: missing field` errors on multi-dimensional or stripped schemas (fixes #336, porting decolua/9router#4798).

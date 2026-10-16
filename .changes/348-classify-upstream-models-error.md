### Fixes

- **models**: report an unreachable (502) or timed-out (504) upstream error during models discovery instead of a blanket 500 internal server error (fixes #347, porting decolua/9router#4843).

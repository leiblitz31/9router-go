### 416 - merge customModelCaps into static caps map

- Merged `h.customModelCaps(provider, resolved)` into the static capability map in `HandleGetModelCaps`, ensuring imported free models (such as those ending in `-free`) appear with `Free=true` even when a provider has a static catalog.
- Added collision check keeping static catalog entries intact over custom model entries.
- Added `TestHandleGetModelCaps_CustomModelMergeAndStaticPrecedence` in `model_caps_test.go` verifying correct behavior and static precedence.

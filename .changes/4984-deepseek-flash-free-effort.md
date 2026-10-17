### fix(opencode): honor low|high|max reasoning efforts for zen free deepseek-v4.1-flash

Ports upstream decolua/9router#4984. `oc/deepseek-v4.1-flash-free` (OpenCode Zen
free lane) serves exactly three reasoning efforts — **low | high | max** — but
the id fell through to the generic `*deepseek-v4.*` rules, so 9router offered
levels the model does not have and silently rewrote others (`low` → `high`).

**Changes:**
- `internal/providers/thinking_levels.go`: `{Pattern: "*deepseek-v4.1-flash-free*", Levels: []string{"low", "high", "max"}}` added above the generic `*deepseek-v4.*` glob (first-match-wins).
- `ClampDeepseekEffort`: honors the declared level set in the low direction — `low`/`minimal` → `low`, but only when the declared set includes `low`. Models whose set omits it keep the old `low` → `high` mapping.

**Mapping after fix:** `low`→`low`, `minimal`→`low`, `medium`→`high`, `high`→`high`, `xhigh`→`max`, `max`→`max`. Thinking-off still works (no effort on the wire).

package providers

// CursorAutoModel returns the concrete model the Cursor picker's "Auto
// (Server Picks)" alias ("default"/"auto") resolves to.
//
// "default" is a picker alias, not an AgentService model id: forwarding it
// literally gets a 200 with an empty turn (decolua/9router#4692). Upstream
// resolves it through the account's live catalog (defaultOn flags); this
// build carries no Cursor catalog fetch, so the registry's first concrete
// entry stands in. Fail-open: an empty registry keeps the literal id,
// which is today's behaviour.
//
// Ponytail: swap this for the live usable-models defaultOn pick when a
// Cursor catalog fetch lands here.
func CursorAutoModel() string {
	for _, m := range ProviderModels["cursor"] {
		if m == "default" || m == "auto" {
			continue
		}
		return m
	}
	return "default"
}

// ResolveCursorAutoModel maps the Cursor "default"/"auto" picker alias to a
// concrete model id. Any other provider or model id passes through.
func ResolveCursorAutoModel(provider, model string) string {
	if provider != "cursor" {
		return model
	}
	if model != "default" && model != "auto" {
		return model
	}
	return CursorAutoModel()
}

package schedule

import "testing"

// The local dashboard persists the schedules half under its own key
// (claudeAutoPingSchedules). The scheduler must still see those schedules,
// with the canonical key winning per connection.
func TestFoldLegacySchedules(t *testing.T) {
	cfg := ParseConfig(map[string]any{
		"connections": map[string]any{"conn-a": true},
		"schedules": map[string]any{
			"conn-a": map[string]any{"enabled": true, "anchor": "08:00", "timezone": "UTC", "windowHours": 5, "slotsPerDay": 1},
		},
	})
	legacy := map[string]any{
		"conn-a": map[string]any{"enabled": true, "anchor": "10:15", "timezone": "UTC", "windowHours": 5, "slotsPerDay": 1},
		"conn-b": map[string]any{"enabled": true, "anchor": "10:15", "timezone": "UTC", "windowHours": 5, "slotsPerDay": 1},
		"junk":   "not-a-schedule",
	}
	foldLegacySchedules(&cfg, legacy)
	if got := cfg.Schedules["conn-a"]; got == nil || got.Anchor != "08:00" {
		t.Errorf("canonical schedule must win for conn-a, got %+v", got)
	}
	if got := cfg.Schedules["conn-b"]; got == nil || got.Anchor != "10:15" {
		t.Errorf("legacy schedule must be folded for conn-b, got %+v", got)
	}
	if _, ok := cfg.Schedules["junk"]; ok {
		t.Error("non-object legacy entry must be dropped")
	}
}

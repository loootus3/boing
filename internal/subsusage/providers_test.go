package subsusage

import (
	"testing"
	"time"
)

func TestParseClaudeUsage(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":12.5,"resets_at":"2026-11-05T20:00:00.123456+00:00"},
"seven_day":{"utilization":40,"resets_at":"2026-11-09T10:00:00Z"},"seven_day_opus":{"utilization":100,"resets_at":null},
"seven_day_sonnet":null,"extra_usage":{"is_enabled":false}}`)
	var e Entry
	if err := parseClaudeUsage(body, &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Windows) != 3 {
		t.Fatalf("windows = %+v", e.Windows)
	}
	if e.Windows[0].Kind != "short" || e.Windows[0].UsedPercent != 12.5 || e.Windows[0].ResetAt == nil {
		t.Fatalf("five_hour = %+v", e.Windows[0])
	}
	w := e.weekly()
	if w == nil || w.ResetAt == nil || !w.ResetAt.Equal(time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly = %+v", w)
	}
	// An exhausted per-model window must not mark the whole account exhausted.
	if e.resetInfo().Exhausted {
		t.Fatal("opus-only exhaustion should not exhaust the account")
	}
}

func TestParseCodexUsage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	body := []byte(`{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,
"primary_window":{"used_percent":7,"limit_window_seconds":18000,"reset_after_seconds":3600},
"secondary_window":{"used_percent":55,"limit_window_seconds":604800,"reset_at":1800300000}}}`)
	var e Entry
	if err := parseCodexUsage(body, &e, now); err != nil {
		t.Fatal(err)
	}
	if e.Plan != "Plus" || len(e.Windows) != 2 {
		t.Fatalf("entry = %+v", e)
	}
	if e.Windows[0].Label != "5h" || !e.Windows[0].ResetAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("primary = %+v", e.Windows[0])
	}
	if w := e.weekly(); w == nil || w.UsedPercent != 55 || w.ResetAt.Unix() != 1800300000 {
		t.Fatalf("weekly = %+v", w)
	}
}

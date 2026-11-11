package subsusage

import (
	"context"
	"testing"
	"time"
)

var resetNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func TestParseResetCreditsByKind(t *testing.T) {
	// Same grants as T3 Code's "offers each kind of reset by its own grant" test.
	body := []byte(`{"cedar_ember":{"eligible":true,"next_grant_id":"weekly_a","grants":[
		{"id":"weekly_a","resets_left":1,"usable_now":true,"clears":["seven_day"],"ends_at":"2026-12-10T22:00:00Z"},
		{"id":"fable_a","resets_left":1,"usable_now":true,"clears":["seven_day_overage_included"],"gives_back_weekly":true,"ends_at":"2027-03-09T07:59:00Z"},
		{"id":"fable_b","resets_left":1,"usable_now":true,"gives_back_weekly":true,"type":"weekly"},
		{"id":"session_a","resets_left":1,"usable_now":true,"type":"session"},
		{"id":"spent","resets_left":0,"usable_now":true,"clears":["seven_day"]},
		{"id":"paused","resets_left":5,"usable_now":true,"paused":true},
		{"id":"old","resets_left":5,"usable_now":true,"ends_at":"2026-11-01T00:00:00Z"},
		{"id":"BAD ID","resets_left":5,"usable_now":true}]}}`)
	rc := parseResetCredits(body, resetNow)
	if rc == nil || rc.Available != 4 || rc.NextExpiresAt == nil || rc.NextExpiresAt.Day() != 10 {
		t.Fatalf("got %+v", rc)
	}
	want := []struct {
		kind, grant string
		n           int
	}{{"session", "session_a", 1}, {"weekly", "weekly_a", 1}, {"fable", "fable_a", 2}}
	if len(rc.Options) != len(want) {
		t.Fatalf("options %+v", rc.Options)
	}
	for i, w := range want {
		o := rc.Options[i]
		if o.Kind != w.kind || o.grantID != w.grant || o.Available != w.n || o.Label == "" {
			t.Fatalf("option %d = %+v, want %+v", i, o, w)
		}
	}
	// A grant with no type and clears on both windows is a full reset.
	rc = parseResetCredits([]byte(`{"cedar_ember":{"eligible":true,"next_grant_id":"g","grants":[{"id":"g","resets_left":2,"usable_now":true}]}}`), resetNow)
	if rc == nil || len(rc.Options) != 1 || rc.Options[0].Kind != "full" || rc.Options[0].Available != 2 {
		t.Fatalf("got %+v", rc)
	}
}

func TestParseResetCreditsNothing(t *testing.T) {
	if parseResetCredits([]byte(`{"cedar_ember":{"eligible":false}}`), resetNow) != nil {
		t.Fatal("ineligible should be nil")
	}
	if parseResetCredits([]byte(`{}`), resetNow) != nil {
		t.Fatal("missing block should be nil")
	}
	// The next grant is not usable: nothing to redeem.
	rc := parseResetCredits([]byte(`{"cedar_ember":{"eligible":true,"next_grant_id":"x","grants":[{"id":"g_2","resets_left":2,"usable_now":true}]}}`), resetNow)
	if rc == nil || rc.Available != 0 || len(rc.Options) != 0 {
		t.Fatalf("got %+v", rc)
	}
}

// Redeeming never reaches the network in tests: without a manager or a matching credit it stops
// before any request.
func TestRedeemWithoutCreditMakesNoRequest(t *testing.T) {
	tr := &Tracker{entries: map[string]*Entry{}}
	if _, _, err := tr.RedeemResetCredit(context.Background(), "a", "weekly"); err == nil {
		t.Fatal("want error without a manager")
	}
}

func TestMarkSpentHidesGrantUntilRefetch(t *testing.T) {
	rc := &ResetCredits{Available: 3, Options: []ResetOption{
		{Kind: "weekly", Available: 2, grantID: "w"},
		{Kind: "fable", Available: 1, grantID: "f"},
	}}
	tr := &Tracker{entries: map[string]*Entry{"a": {AuthID: "a", ResetCredits: rc}}}
	tr.markSpent("a", "w")
	got := tr.entries["a"].ResetCredits
	if got.Available != 2 || len(got.Options) != 1 || got.Options[0].Kind != "fable" {
		t.Fatalf("got %+v", got)
	}
	if rc.Available != 3 || len(rc.Options) != 2 {
		t.Fatal("the old snapshot must stay untouched")
	}
	// A redemption of the spent kind now finds no grant and stops before any request.
	if _, _, err := tr.RedeemResetCredit(context.Background(), "a", "weekly"); err == nil {
		t.Fatal("want error without a manager")
	}
}

func TestResetInfoFableExhausted(t *testing.T) {
	now := time.Now()
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	pct := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		name  string
		limit *float64
		reset *time.Time
		want  bool
	}{
		{"full, resets later", pct(100), &future, true},
		{"full, already reset", pct(100), &past, false},
		{"full, no reset time", pct(100), nil, true},
		{"room left", pct(99), &future, false},
		{"no fable data", nil, &future, false},
	} {
		e := &Entry{Windows: []Window{{Kind: "weekly", UsedPercent: 50, FableLimitPercent: c.limit, FableResetAt: c.reset}}}
		info := e.resetInfo()
		if info.FableExhausted != c.want || info.Exhausted {
			t.Errorf("%s: got %+v", c.name, info)
		}
	}
}

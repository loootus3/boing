package subsusage

import (
	"context"
	"fmt"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestIsHotAndIsDue(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if !isHot(now.Add(-14*time.Minute), false, now) || isHot(now.Add(-16*time.Minute), false, now) {
		t.Fatal("recent use should be hot, old use idle")
	}
	if isHot(time.Time{}, false, now) || !isHot(time.Time{}, true, now) {
		t.Fatal("zero last use is idle unless active")
	}
	cases := []struct {
		name     string
		hasEntry bool
		fast     bool
		hot      bool
		since    time.Duration
		want     bool
	}{
		{"new auth", false, false, false, 0, true},
		{"hot not yet", true, false, true, 119 * time.Second, false},
		{"hot due", true, false, true, 120 * time.Second, true},
		{"idle not yet", true, false, false, 9 * time.Minute, false},
		{"idle due", true, false, false, 10 * time.Minute, true},
		{"idle fast retry not yet", true, true, false, 119 * time.Second, false},
		{"idle fast retry due", true, true, false, 120 * time.Second, true},
	}
	for _, c := range cases {
		if got := isDue(c.hasEntry, c.fast, c.hot, now.Add(-c.since), now); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRetriesFast(t *testing.T) {
	win := []Window{{Kind: "weekly"}}
	cases := []struct {
		name string
		e    *Entry
		want bool
	}{
		{"nil", nil, false},
		{"has windows", &Entry{Windows: win, Error: "HTTP 429"}, false},
		{"no error", &Entry{}, true},
		{"rate limit", &Entry{Error: "HTTP 429"}, true},
		{"network", &Entry{Error: "Get \"https://x\": dial tcp: i/o timeout"}, true},
		{"unauthorized", &Entry{Error: "HTTP 401"}, false},
		{"needs login", &Entry{Error: "boom", NeedsLogin: true}, false},
		{"no token", &Entry{Error: "sem access token"}, false},
		{"forbidden", &Entry{Error: "HTTP 403"}, false},
		{"server error", &Entry{Error: "HTTP 500"}, false},
	}
	for _, c := range cases {
		if got := retriesFast(c.e); got != c.want {
			t.Errorf("%s: retriesFast = %v, want %v", c.name, got, c.want)
		}
	}
}

type fakeFetcher struct {
	calls []string
	errs  map[string]error
}

func (f *fakeFetcher) fetch(_ context.Context, a *coreauth.Auth) (*Entry, error) {
	f.calls = append(f.calls, a.ID)
	e := &Entry{AuthID: a.ID, Provider: "claude", Windows: []Window{{Kind: "weekly", UsedPercent: 10}}}
	if err := f.errs[a.ID]; err != nil {
		e.Windows = nil
		e.Error = err.Error()
		return e, err
	}
	e.DataAt = time.Now()
	return e, nil
}

func newFakeTracker(f *fakeFetcher, clock *time.Time, pauses *int) *Tracker {
	return &Tracker{
		entries:    make(map[string]*Entry),
		fetchFn:    f.fetch,
		nowFn:      func() time.Time { return *clock },
		pauseFn:    func(context.Context, time.Duration) error { *pauses++; return nil },
		lastUsedFn: func(string) time.Time { return time.Time{} },
	}
}

func claudeAuth(id string) *coreauth.Auth { return &coreauth.Auth{ID: id, Provider: "claude"} }

func TestRefreshSequentialHotFirstAndCarry(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeFetcher{}
	pauses := 0
	tr := newFakeTracker(f, &clock, &pauses)
	used := map[string]time.Time{"c": clock}
	tr.lastUsedFn = func(id string) time.Time { return used[id] }
	auths := []*coreauth.Auth{claudeAuth("a"), claudeAuth("b"), claudeAuth("c")}

	// First round: all new, so all due. "a" is the active account and "c" was used recently:
	// both are hot and go before idle "b". Two pauses between three requests.
	tr.refreshAuths(context.Background(), true, auths)
	if fmt.Sprint(f.calls) != "[a c b]" || pauses != 2 {
		t.Fatalf("calls = %v pauses = %d", f.calls, pauses)
	}

	// 121s later only the hot accounts are due; the idle one is carried unchanged.
	f.calls = nil
	clock = clock.Add(121 * time.Second)
	used["c"] = clock
	before := tr.entries["b"]
	tr.refreshAuths(context.Background(), true, auths)
	if fmt.Sprint(f.calls) != "[a c]" {
		t.Fatalf("calls = %v", f.calls)
	}
	if tr.entries["b"] != before || len(tr.entries) != 3 {
		t.Fatal("idle entry not carried unchanged")
	}

	// 10 minutes later everyone is due again.
	f.calls = nil
	clock = clock.Add(10 * time.Minute)
	tr.refreshAuths(context.Background(), true, auths)
	if len(f.calls) != 3 {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestRefreshStopsRoundOn429(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeFetcher{errs: map[string]error{"a": &httpStatusError{Code: 429}}}
	pauses := 0
	tr := newFakeTracker(f, &clock, &pauses)
	auths := []*coreauth.Auth{claudeAuth("a"), claudeAuth("b"), claudeAuth("c")}
	tr.refreshAuths(context.Background(), true, auths)

	if len(f.calls) != 1 || f.calls[0] != "a" {
		t.Fatalf("calls = %v", f.calls)
	}
	if e := tr.entries["a"]; e == nil || e.RetryAt == nil || !e.RetryAt.Equal(clock.Add(firstBackoff)) {
		t.Fatalf("entry a = %+v", e)
	}
	if _, ok := tr.backoff["b"]; ok {
		t.Fatal("unreached auth was penalized")
	}
	for _, id := range []string{"b", "c"} {
		e := tr.entries[id]
		if e == nil || e.RetryAt != nil || e.Error != "" {
			t.Fatalf("entry %s = %+v", id, e)
		}
		if _, ok := tr.lastFetch[id]; ok {
			t.Fatalf("lastFetch set for unreached %s", id)
		}
	}

	// Unreached auths are still due on the next round; "a" stays in backoff.
	f.calls = nil
	f.errs = nil
	clock = clock.Add(time.Minute)
	tr.refreshAuths(context.Background(), true, auths)
	if fmt.Sprint(f.calls) != "[b c]" {
		t.Fatalf("calls = %v", f.calls)
	}
	if e := tr.entries["a"]; e.RetryAt == nil {
		t.Fatal("backoff lost")
	}
}

func TestRefreshPauseCancelCarriesRest(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeFetcher{}
	pauses := 0
	tr := newFakeTracker(f, &clock, &pauses)
	tr.pauseFn = func(context.Context, time.Duration) error { return context.Canceled }
	auths := []*coreauth.Auth{claudeAuth("a"), claudeAuth("b")}
	tr.refreshAuths(context.Background(), true, auths)
	if len(f.calls) != 1 || len(tr.entries) != 2 {
		t.Fatalf("calls = %v entries = %d", f.calls, len(tr.entries))
	}
}

func TestHistoryLastUsed(t *testing.T) {
	h := &History{data: newHistoryData()}
	if !h.LastUsed("a").IsZero() {
		t.Fatal("expected zero")
	}
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.HandleUsage(context.Background(), usage.Record{Provider: "claude", AuthID: "a", RequestedAt: at})
	if !h.LastUsed("a").Equal(at) {
		t.Fatalf("LastUsed = %v", h.LastUsed("a"))
	}
}

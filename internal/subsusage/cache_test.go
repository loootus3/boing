package subsusage

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCachedAPICall(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	tr := &Tracker{nowFn: func() time.Time { return now }}
	claude := &coreauth.Auth{ID: "c1", Provider: "Claude"}
	body := []byte(`{"five_hour":{"utilization":12,"resets_at":null}}`)
	tr.storeRawUsage("c1", body)

	parse := func(s string) *url.URL {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	usage := parse(claudeUsageURL)
	if got, ok := tr.CachedAPICall(claude, http.MethodGet, usage); !ok || string(got) != string(body) {
		t.Fatalf("exact usage GET: ok=%v body=%q", ok, got)
	}
	if _, ok := tr.CachedAPICall(claude, http.MethodGet, parse("HTTPS://API.ANTHROPIC.COM/api/oauth/usage")); !ok {
		t.Fatal("scheme and host should match case-insensitively")
	}
	for name, tc := range map[string]struct {
		auth   *coreauth.Auth
		method string
		target *url.URL
	}{
		"nil auth":       {nil, http.MethodGet, usage},
		"codex auth":     {&coreauth.Auth{ID: "c1", Provider: "codex"}, http.MethodGet, usage},
		"other account":  {&coreauth.Auth{ID: "c2", Provider: "claude"}, http.MethodGet, usage},
		"post":           {claude, http.MethodPost, usage},
		"query":          {claude, http.MethodGet, parse(claudeUsageURL + "?cedar_ember=1")},
		"other path":     {claude, http.MethodGet, parse(claudeProfileURL)},
		"other host":     {claude, http.MethodGet, parse("https://example.com/api/oauth/usage")},
		"plain http":     {claude, http.MethodGet, parse("http://api.anthropic.com/api/oauth/usage")},
		"userinfo":       {claude, http.MethodGet, parse("https://x@api.anthropic.com/api/oauth/usage")},
		"nil target url": {claude, http.MethodGet, nil},
	} {
		if _, ok := tr.CachedAPICall(tc.auth, tc.method, tc.target); ok {
			t.Errorf("%s: served from cache, want upstream", name)
		}
	}

	now = now.Add(staleAfter + time.Second)
	if _, ok := tr.CachedAPICall(claude, http.MethodGet, usage); ok {
		t.Fatal("a copy older than staleAfter must not be served")
	}
}

func TestRefreshPrunesRawUsage(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	tr := &Tracker{
		entries:    map[string]*Entry{},
		nowFn:      func() time.Time { return now },
		pauseFn:    func(context.Context, time.Duration) error { return nil },
		lastUsedFn: func(string) time.Time { return time.Time{} },
		fetchFn: func(_ context.Context, a *coreauth.Auth) (*Entry, error) {
			return &Entry{AuthID: a.ID, Provider: "claude", Windows: []Window{{Kind: "weekly", UsedPercent: 1}}}, nil
		},
	}
	tr.storeRawUsage("kept", []byte(`{}`))
	tr.storeRawUsage("removed", []byte(`{}`))
	tr.refreshAuths(context.Background(), true, []*coreauth.Auth{{ID: "kept", Provider: "claude"}})
	if _, ok := tr.rawUsage["kept"]; !ok {
		t.Fatal("copy of a tracked account was dropped")
	}
	if _, ok := tr.rawUsage["removed"]; ok {
		t.Fatal("copy of a removed account was kept")
	}
}

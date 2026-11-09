package subsusage

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestStaleCooldowns(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	paused := func(id string) *coreauth.Auth {
		return &coreauth.Auth{ID: id, Provider: "claude", Quota: coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(48 * time.Hour)}}
	}
	free := &Entry{Windows: []Window{{Kind: "weekly", UsedPercent: 40}}}
	full := &Entry{Windows: []Window{{Kind: "weekly", UsedPercent: 100}}}
	modelOnly := &coreauth.Auth{ID: "model", Provider: "claude", Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)}}
	expired := paused("expired")
	expired.Quota.NextRecoverAt = now.Add(-time.Minute)
	auths := []*coreauth.Auth{paused("freed"), paused("still-full"), paused("not-fetched"), paused("errored"), paused("recent"), modelOnly, expired}
	fetched := map[string]*Entry{
		"freed": free, "still-full": full, "errored": {Error: "HTTP 500", Windows: free.Windows},
		"recent": free, "model": free, "expired": free,
	}
	got := staleCooldowns(auths, fetched, now, map[string]time.Time{"recent": now.Add(-time.Minute)})
	if len(got) != 1 || got[0] != "freed" {
		t.Fatalf("got %v, want [freed]", got)
	}
	if got := staleCooldowns(auths[4:5], fetched, now, map[string]time.Time{"recent": now.Add(-cooldownClearGap)}); len(got) != 1 {
		t.Fatalf("clear gap elapsed: got %v", got)
	}
}

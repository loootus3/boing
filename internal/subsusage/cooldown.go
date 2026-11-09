package subsusage

import (
	"context"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// cooldownClearGap is the minimum time between two cooldown clears of the same account, so an
// account whose usage reads as free while the API still refuses it costs at most one request
// per gap.
const cooldownClearGap = 30 * time.Minute

// credentialCooling reports whether the proxy has paused the whole credential after a quota
// error (shared 5h/weekly limit), as opposed to a single model.
func credentialCooling(a *coreauth.Auth, now time.Time) bool {
	return a != nil && a.Quota.Exceeded && a.Quota.Reason == "credential_quota" && a.Quota.NextRecoverAt.After(now)
}

// staleCooldowns returns the accounts that were paused at startedAt but whose usage, fetched in
// the round that started then (so after the pause began), shows room in every shared window.
func staleCooldowns(auths []*coreauth.Auth, fetched map[string]*Entry, startedAt time.Time, lastCleared map[string]time.Time) []string {
	var ids []string
	for _, a := range auths {
		if !credentialCooling(a, startedAt) {
			continue
		}
		e := fetched[a.ID]
		if e == nil || e.Error != "" || len(e.Windows) == 0 {
			continue
		}
		if e.resetInfo().Exhausted {
			continue
		}
		if at, ok := lastCleared[a.ID]; ok && startedAt.Sub(at) < cooldownClearGap {
			continue
		}
		ids = append(ids, a.ID)
	}
	return ids
}

func (t *Tracker) clearStaleCooldowns(ctx context.Context, auths []*coreauth.Auth, fetched map[string]*Entry, startedAt time.Time) {
	t.mu.RLock()
	m := t.manager
	ids := staleCooldowns(auths, fetched, startedAt, t.cooldownCleared)
	t.mu.RUnlock()
	if m == nil || len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if _, _, err := m.ResetQuota(ctx, id); err != nil {
			log.Warnf("subsusage: clear cooldown of %s: %v", id, err)
			continue
		}
		log.Infof("subsusage: %s has quota again; cleared its routing cooldown", id)
		t.mu.Lock()
		if t.cooldownCleared == nil {
			t.cooldownCleared = make(map[string]time.Time)
		}
		t.cooldownCleared[id] = startedAt
		t.mu.Unlock()
	}
}

package auth

import (
	"context"
	"sort"
	"sync/atomic"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetInfo describes the subscription window state of one credential as observed by an
// external usage tracker (for example the local /usage dashboard poller).
type ResetInfo struct {
	// ResetAt is when the credential's long (weekly) subscription window resets.
	ResetAt time.Time
	// Exhausted reports that at least one window is fully used (>= 100%).
	Exhausted bool
}

// ResetInfoFunc returns the latest known ResetInfo for an auth ID.
type ResetInfoFunc func(authID string) (ResetInfo, bool)

var resetInfoProvider atomic.Pointer[ResetInfoFunc]

// SetResetInfoProvider installs the source of reset information used by SoonestResetSelector.
// Passing nil removes it, making the selector behave like fill-first.
func SetResetInfoProvider(fn ResetInfoFunc) {
	if fn == nil {
		resetInfoProvider.Store(nil)
		return
	}
	resetInfoProvider.Store(&fn)
}

func lookupResetInfo(authID string) (ResetInfo, bool) {
	fn := resetInfoProvider.Load()
	if fn == nil || *fn == nil {
		return ResetInfo{}, false
	}
	return (*fn)(authID)
}

// AuthAccessToken returns the bearer access token stored on an OAuth credential.
func AuthAccessToken(a *Auth) string {
	return authAccessToken(a)
}

// SoonestResetSelector spends the credential whose subscription window resets first, so
// quota that is about to expire is used before quota that would carry on for longer.
// Exhausted credentials go last, and credentials without known reset data sit in between.
// Priority tiers and cooldowns are honoured exactly like FillFirstSelector.
type SoonestResetSelector struct{}

// Pick selects the available auth with the nearest subscription reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	ordered := SortBySoonestReset(available, now)
	return ordered[0], nil
}

// SortBySoonestReset returns a copy of auths ordered by spending preference.
func SortBySoonestReset(auths []*Auth, now time.Time) []*Auth {
	type ranked struct {
		auth    *Auth
		class   int
		resetAt time.Time
	}
	items := make([]ranked, 0, len(auths))
	for _, a := range auths {
		if a == nil {
			continue
		}
		item := ranked{auth: a, class: 1}
		if info, ok := lookupResetInfo(a.ID); ok {
			switch {
			case info.Exhausted:
				item.class = 2
			case !info.ResetAt.IsZero() && info.ResetAt.After(now):
				item.class = 0
				item.resetAt = info.ResetAt
			}
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].class != items[j].class {
			return items[i].class < items[j].class
		}
		if items[i].class == 0 && !items[i].resetAt.Equal(items[j].resetAt) {
			return items[i].resetAt.Before(items[j].resetAt)
		}
		return false
	})
	out := make([]*Auth, 0, len(items))
	for _, it := range items {
		out = append(out, it.auth)
	}
	return out
}

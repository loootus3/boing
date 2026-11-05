// Package subsusage polls the subscription usage windows of Claude and Codex OAuth
// credentials and exposes them to the soonest-reset selector and the local /usage page.
package subsusage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	pollInterval    = 60 * time.Second
	minRefreshDelay = 20 * time.Second
	// staleAfter bounds how long windows from the last successful fetch are reused. It must
	// outlast maxBackoff so the last good windows survive a full rate-limit backoff.
	staleAfter     = 30 * time.Minute
	requestTimeout = 20 * time.Second
	// planTTL is how long a Claude plan read from the profile endpoint is reused.
	planTTL = 6 * time.Hour

	firstBackoff = 2 * time.Minute
	maxBackoff   = 15 * time.Minute

	// hotWindow is how long after a proxied request an account counts as in use.
	hotWindow = 15 * time.Minute
	// hotInterval and idleInterval are the minimum time between fetches of in-use and idle accounts.
	hotInterval  = 60 * time.Second
	idleInterval = 10 * time.Minute
	// fetchGap is the pause between two sequential usage requests in one round.
	fetchGap = 2 * time.Second
)

// httpStatusError is returned by get for any non-200 response.
type httpStatusError struct{ Code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

func isRateLimited(err error) bool {
	var statusErr *httpStatusError
	return errors.As(err, &statusErr) && statusErr.Code == http.StatusTooManyRequests
}

// nextBackoff returns the wait after a rate-limit response: 2m first, then doubling up to 15m.
func nextBackoff(prev time.Duration) time.Duration {
	if prev <= 0 {
		return firstBackoff
	}
	next := prev * 2
	if next > maxBackoff {
		next = maxBackoff
	}
	return next
}

// backoffState tracks the rate-limit backoff of one credential.
type backoffState struct {
	step  time.Duration
	until time.Time
}

// cachedPlan is a plan name together with the time it was read.
type cachedPlan struct {
	plan string
	at   time.Time
}

// carryEntry copies the previous entry for a credential that is skipped while backing off.
// Windows older than staleAfter are dropped like in the failure path.
func carryEntry(prev *Entry, until, now time.Time) *Entry {
	e := *prev
	e.Windows = append([]Window(nil), prev.Windows...)
	if now.Sub(prev.DataAt) > staleAfter {
		e.Windows = nil
	}
	e.RetryAt = &until
	return &e
}

// Window is one usage window of a subscription.
type Window struct {
	Kind        string     `json:"kind"` // "short" (5h), "weekly", "monthly" or "model" (per-model weekly)
	Label       string     `json:"label"`
	UsedPercent float64    `json:"used_percent"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
	// DurationSec is the window length, used by the page to draw the even-pace marker.
	DurationSec int64 `json:"duration_sec,omitempty"`
}

// Entry is the usage snapshot of one credential.
type Entry struct {
	AuthID    string    `json:"auth_id"`
	Provider  string    `json:"provider"`
	Email     string    `json:"email"`
	Plan      string    `json:"plan,omitempty"`
	Disabled  bool      `json:"disabled"`
	Windows   []Window  `json:"windows"`
	FetchedAt time.Time `json:"fetched_at"`
	DataAt    time.Time `json:"data_at"` // time of the last successful fetch backing Windows
	Error     string    `json:"error,omitempty"`
	Active    bool      `json:"active"` // next credential the soonest-reset selector will spend
	// Mode is the spending mode: "normal", "last" or "off" (see coreauth.MetadataUsageMode).
	Mode string `json:"mode"`
	// NeedsLogin is set when the provider rejected the stored token.
	NeedsLogin bool `json:"needs_login,omitempty"`
	// LimitReached is set when the upstream reports the subscription limit as reached.
	LimitReached bool `json:"limit_reached"`
	// RetryAt is set while the credential is backing off after an upstream rate limit.
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

// weekly returns the main long window: weekly, or monthly for plans without one.
func (e *Entry) weekly() *Window {
	for _, kind := range []string{"weekly", "monthly"} {
		for i := range e.Windows {
			if e.Windows[i].Kind == kind {
				return &e.Windows[i]
			}
		}
	}
	return nil
}

func (e *Entry) resetInfo() coreauth.ResetInfo {
	info := coreauth.ResetInfo{Exhausted: e.LimitReached}
	now := time.Now()
	for _, w := range e.Windows {
		// A window whose reset time has passed is no longer exhausted.
		// Per-model windows (e.g. Opus only) do not block the whole account.
		if w.Kind != "model" && w.UsedPercent >= 100 && (w.ResetAt == nil || w.ResetAt.After(now)) {
			info.Exhausted = true
		}
	}
	if w := e.weekly(); w != nil && w.ResetAt != nil {
		info.ResetAt = *w.ResetAt
	}
	return info
}

// Tracker periodically fetches usage for all Claude and Codex credentials.
type Tracker struct {
	mu          sync.RWMutex
	manager     *coreauth.Manager
	entries     map[string]*Entry
	lastRefresh time.Time
	refreshMu   sync.Mutex
	client      *http.Client
	startOnce   sync.Once
	// backoff and plans are keyed by auth ID and guarded by mu.
	backoff map[string]backoffState
	plans   map[string]cachedPlan
	// lastFetch holds the time of the last fetch attempt per auth ID, guarded by mu.
	lastFetch map[string]time.Time

	// Test seams; nil means the production implementation.
	fetchFn    func(ctx context.Context, a *coreauth.Auth) (*Entry, error)
	nowFn      func() time.Time
	pauseFn    func(ctx context.Context, d time.Duration) error
	lastUsedFn func(authID string) time.Time
}

var defaultTracker = &Tracker{
	entries: make(map[string]*Entry),
	client:  &http.Client{Timeout: requestTimeout, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
}

// Default returns the process-wide tracker.
func Default() *Tracker { return defaultTracker }

// Start binds the tracker to an auth manager and starts the poll loop once per process.
func (t *Tracker) Start(manager *coreauth.Manager) {
	if manager == nil {
		return
	}
	t.mu.Lock()
	t.manager = manager
	t.mu.Unlock()
	t.startOnce.Do(func() {
		coreauth.SetResetInfoProvider(t.lookup)
		go t.loop()
	})
}

func (t *Tracker) lookup(authID string) (coreauth.ResetInfo, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[authID]
	if !ok || len(e.Windows) == 0 || time.Since(e.DataAt) > staleAfter {
		return coreauth.ResetInfo{}, false
	}
	return e.resetInfo(), true
}

func (t *Tracker) loop() {
	// Give the auth manager a moment to load credentials from disk.
	time.Sleep(3 * time.Second)
	for {
		t.Refresh(context.Background(), true)
		time.Sleep(pollInterval)
	}
}

func (t *Tracker) now() time.Time {
	if t.nowFn != nil {
		return t.nowFn()
	}
	return time.Now()
}

func (t *Tracker) doFetch(ctx context.Context, a *coreauth.Auth) (*Entry, error) {
	if t.fetchFn != nil {
		return t.fetchFn(ctx, a)
	}
	return t.fetch(ctx, a)
}

func (t *Tracker) pause(ctx context.Context, d time.Duration) error {
	if t.pauseFn != nil {
		return t.pauseFn(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *Tracker) lastUsed(authID string) time.Time {
	if t.lastUsedFn != nil {
		return t.lastUsedFn(authID)
	}
	return defaultHistory.LastUsed(authID)
}

// isHot reports whether an account is in use: it served a proxied request recently or it is
// the account the selector spends next.
func isHot(lastUsed time.Time, active bool, now time.Time) bool {
	return active || (!lastUsed.IsZero() && now.Sub(lastUsed) <= hotWindow)
}

// isDue reports whether an account should be fetched now. hasEntry is false for accounts
// that were never fetched; those are always due.
func isDue(hasEntry, hot bool, lastFetch, now time.Time) bool {
	if !hasEntry {
		return true
	}
	interval := idleInterval
	if hot {
		interval = hotInterval
	}
	return now.Sub(lastFetch) >= interval
}

// Refresh fetches usage for the tracked credentials that are due. Accounts in use are polled
// every hotInterval, idle ones every idleInterval, one request at a time. Without force,
// refreshes closer together than minRefreshDelay are skipped.
func (t *Tracker) Refresh(ctx context.Context, force bool) {
	t.mu.RLock()
	manager := t.manager
	t.mu.RUnlock()
	if manager == nil {
		return
	}
	t.refreshAuths(ctx, force, trackedAuths(manager.List()))
}

func (t *Tracker) refreshAuths(ctx context.Context, force bool, auths []*coreauth.Auth) {
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()

	startedAt := t.now()
	if !force && startedAt.Sub(t.lastRefresh) < minRefreshDelay && !t.hasNewAuths(auths) {
		return
	}
	// Order first: it reads entries through t.lookup, which takes its own read lock.
	groups := t.orderedGroups(auths, startedAt)

	type pending struct {
		auth *coreauth.Auth
		hot  bool
	}
	results := make(map[string]*Entry, len(auths))
	skipped := make(map[string]time.Time)
	var due []pending

	t.mu.RLock()
	active := t.activeIDsLocked(groups)
	for _, a := range auths {
		if a.Disabled {
			continue
		}
		prev, hasPrev := t.entries[a.ID]
		if b, ok := t.backoff[a.ID]; ok && startedAt.Before(b.until) {
			skipped[a.ID] = b.until
			continue
		}
		hot := isHot(t.lastUsed(a.ID), active[a.ID], startedAt)
		if !isDue(hasPrev, hot, t.lastFetch[a.ID], startedAt) {
			results[a.ID] = prev
			continue
		}
		due = append(due, pending{auth: a, hot: hot})
	}
	t.mu.RUnlock()
	sort.SliceStable(due, func(i, j int) bool { return due[i].hot && !due[j].hot })

	fetched := make(map[string]*Entry, len(due))
	errs := make(map[string]error)
	attempted := make(map[string]time.Time, len(due))
	var rest []pending
	for i, p := range due {
		if i > 0 {
			if err := t.pause(ctx, fetchGap); err != nil {
				rest = due[i:]
				break
			}
		}
		entry, err := t.doFetch(ctx, p.auth)
		attempted[p.auth.ID] = t.now()
		fetched[p.auth.ID] = entry
		if err != nil {
			errs[p.auth.ID] = err
			if isRateLimited(err) {
				// The limit looks per client: stop the round and leave the rest untouched.
				rest = due[i+1:]
				break
			}
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.backoff == nil {
		t.backoff = make(map[string]backoffState)
	}
	if t.lastFetch == nil {
		t.lastFetch = make(map[string]time.Time)
	}
	for id, at := range attempted {
		t.lastFetch[id] = at
	}
	for id, e := range fetched {
		// Keep the last good windows when a fetch fails transiently.
		if e.Error != "" {
			if prev, ok := t.entries[id]; ok && len(prev.Windows) > 0 && now.Sub(prev.DataAt) <= staleAfter {
				e.Windows = prev.Windows
				e.DataAt = prev.DataAt
				e.LimitReached = prev.LimitReached
				if e.Plan == "" {
					e.Plan = prev.Plan
				}
			}
		}
		switch err := errs[id]; {
		case err == nil:
			delete(t.backoff, id)
		case isRateLimited(err):
			step := nextBackoff(t.backoff[id].step)
			until := now.Add(step)
			t.backoff[id] = backoffState{step: step, until: until}
			e.RetryAt = &until
		}
		results[id] = e
	}
	// Due accounts the round never reached keep their previous entry.
	for _, p := range rest {
		id := p.auth.ID
		if prev, ok := t.entries[id]; ok {
			results[id] = prev
			continue
		}
		e := &Entry{AuthID: id, Provider: strings.ToLower(p.auth.Provider), FetchedAt: now}
		_, e.Email = p.auth.AccountInfo()
		results[id] = e
	}
	// Credentials still backing off are not fetched: reuse their previous entry.
	for id, until := range skipped {
		var sk *coreauth.Auth
		for _, a := range auths {
			if a.ID == id {
				sk = a
				break
			}
		}
		if prev, ok := t.entries[id]; ok {
			results[id] = carryEntry(prev, until, now)
			continue
		}
		u := until
		e := &Entry{AuthID: id, Provider: strings.ToLower(sk.Provider), FetchedAt: now, Error: "HTTP 429", RetryAt: &u}
		_, e.Email = sk.AccountInfo()
		results[id] = e
	}
	t.entries = results
	t.lastRefresh = now
	for _, e := range fetched {
		defaultHistory.recordQuota(e, now)
	}
}

func (t *Tracker) hasNewAuths(auths []*coreauth.Auth) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, a := range auths {
		if _, ok := t.entries[a.ID]; !ok {
			return true
		}
	}
	return false
}

func trackedAuths(all []*coreauth.Auth) []*coreauth.Auth {
	out := make([]*coreauth.Auth, 0, len(all))
	for _, a := range all {
		if a == nil {
			continue
		}
		switch strings.ToLower(a.Provider) {
		case "claude", "codex":
			if _, email := a.AccountInfo(); email != "" || coreauth.AuthAccessToken(a) != "" {
				out = append(out, a)
			}
		}
	}
	return out
}

// Snapshot returns all entries ordered for display: Claude first, then Codex, each in the
// order the soonest-reset selector would spend them.
func (t *Tracker) Snapshot() []Entry {
	t.mu.RLock()
	manager := t.manager
	t.mu.RUnlock()
	if manager == nil {
		return nil
	}
	auths := trackedAuths(manager.List())
	now := time.Now()

	// Order first: SortBySoonestReset reads entries through t.lookup, which takes its own
	// read lock. Holding t.mu here would nest read locks and can deadlock with Refresh.
	providers := []string{"claude", "codex"}
	groups := t.orderedGroups(auths, now)

	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Entry, 0, len(auths))
	for _, provider := range providers {
		ordered := groups[provider]
		activeSet := false
		for _, a := range ordered {
			var e Entry
			if cached, ok := t.entries[a.ID]; ok {
				e = *cached
				e.Windows = append([]Window(nil), cached.Windows...)
			} else {
				e = Entry{AuthID: a.ID, Provider: provider}
			}
			_, e.Email = a.AccountInfo()
			if e.Email == "" {
				e.Email = a.Label
			}
			e.Disabled = a.Disabled
			e.Mode = coreauth.AuthUsageMode(a)
			e.NeedsLogin = strings.Contains(e.Error, "HTTP 401") || strings.Contains(strings.ToLower(a.StatusMessage), "invalid_grant")
			if !activeSet && !a.Disabled && e.Mode != coreauth.UsageModeOff && !e.NeedsLogin && !e.resetInfo().Exhausted {
				e.Active = true
				activeSet = true
			}
			out = append(out, e)
		}
	}
	return out
}

// orderedGroups returns the auths per provider in the order the selector would spend them.
// It must not be called with t.mu held.
func (t *Tracker) orderedGroups(auths []*coreauth.Auth, now time.Time) map[string][]*coreauth.Auth {
	providers := []string{"claude", "codex"}
	groups := make(map[string][]*coreauth.Auth, len(providers))
	for _, provider := range providers {
		var group []*coreauth.Auth
		for _, a := range auths {
			if strings.EqualFold(a.Provider, provider) {
				group = append(group, a)
			}
		}
		sort.SliceStable(group, func(i, j int) bool { return group[i].ID < group[j].ID })
		ordered := coreauth.SortBySoonestReset(group, now)
		// Normal accounts are spent before "last" ones; "off" accounts are never spent.
		sort.SliceStable(ordered, func(i, j int) bool {
			return modeRank(coreauth.AuthUsageMode(ordered[i])) < modeRank(coreauth.AuthUsageMode(ordered[j]))
		})
		groups[provider] = ordered
	}
	return groups
}

// canBeActive reports whether the account may be marked as the next one to be spent.
// NeedsLogin is derived here so callers can pass an entry that does not have it set yet.
func canBeActive(a *coreauth.Auth, e *Entry) bool {
	needsLogin := strings.Contains(e.Error, "HTTP 401") || strings.Contains(strings.ToLower(a.StatusMessage), "invalid_grant")
	return !a.Disabled && coreauth.AuthUsageMode(a) != coreauth.UsageModeOff && !needsLogin && !e.resetInfo().Exhausted
}

// activeIDsLocked returns the first spendable account of each provider. t.mu must be held.
func (t *Tracker) activeIDsLocked(groups map[string][]*coreauth.Auth) map[string]bool {
	active := make(map[string]bool, len(groups))
	for _, ordered := range groups {
		for _, a := range ordered {
			e := t.entries[a.ID]
			if e == nil {
				e = &Entry{}
			}
			if canBeActive(a, e) {
				active[a.ID] = true
				break
			}
		}
	}
	return active
}

func modeRank(mode string) int {
	switch mode {
	case coreauth.UsageModeLast:
		return 1
	case coreauth.UsageModeOff:
		return 2
	}
	return 0
}

func (t *Tracker) fetch(ctx context.Context, a *coreauth.Auth) (*Entry, error) {
	entry := &Entry{AuthID: a.ID, Provider: strings.ToLower(a.Provider), FetchedAt: time.Now()}
	_, entry.Email = a.AccountInfo()
	token := coreauth.AuthAccessToken(a)
	if token == "" {
		entry.Error = "sem access token"
		return entry, errors.New(entry.Error)
	}
	var err error
	switch entry.Provider {
	case "claude":
		err = t.fetchClaude(ctx, a.ID, token, entry)
	case "codex":
		err = t.fetchCodex(ctx, token, codexAccountID(a), entry)
	}
	if err == nil {
		entry.DataAt = entry.FetchedAt
	} else {
		entry.Error = err.Error()
		log.WithField("provider", entry.Provider).Debugf("subsusage: fetch failed: %v", err)
	}
	return entry, err
}

// cachedPlanOf returns the cached plan of an auth and when it was read.
func (t *Tracker) cachedPlanOf(authID string) (cachedPlan, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	c, ok := t.plans[authID]
	return c, ok
}

func (t *Tracker) storePlan(authID, plan string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.plans == nil {
		t.plans = make(map[string]cachedPlan)
	}
	t.plans[authID] = cachedPlan{plan: plan, at: time.Now()}
}

func metaString(a *coreauth.Auth, key string) string {
	if a == nil || a.Metadata == nil {
		return ""
	}
	if v, ok := a.Metadata[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func (t *Tracker) get(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("subsusage: close body: %v", errClose)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{Code: resp.StatusCode}
	}
	return body, nil
}

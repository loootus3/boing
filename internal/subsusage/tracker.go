// Package subsusage polls the subscription usage windows of Claude and Codex OAuth
// credentials and exposes them to the soonest-reset selector and the local /usage page.
package subsusage

import (
	"context"
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
	// staleAfter bounds how long windows from the last successful fetch are reused.
	staleAfter     = 15 * time.Minute
	requestTimeout = 20 * time.Second
)

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

// Refresh fetches usage for every tracked credential. Without force, refreshes closer
// together than minRefreshDelay are skipped.
func (t *Tracker) Refresh(ctx context.Context, force bool) {
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	t.mu.RLock()
	manager := t.manager
	t.mu.RUnlock()
	if manager == nil {
		return
	}

	auths := trackedAuths(manager.List())
	if !force && time.Since(t.lastRefresh) < minRefreshDelay && !t.hasNewAuths(auths) {
		return
	}
	results := make(map[string]*Entry, len(auths))
	var wg sync.WaitGroup
	var resMu sync.Mutex
	for _, a := range auths {
		if a.Disabled {
			continue
		}
		wg.Add(1)
		go func(a *coreauth.Auth) {
			defer wg.Done()
			entry := t.fetch(ctx, a)
			resMu.Lock()
			results[a.ID] = entry
			resMu.Unlock()
		}(a)
	}
	wg.Wait()

	t.mu.Lock()
	for id, e := range results {
		// Keep the last good windows when a fetch fails transiently.
		if e.Error != "" {
			if prev, ok := t.entries[id]; ok && len(prev.Windows) > 0 && time.Since(prev.DataAt) <= staleAfter {
				e.Windows = prev.Windows
				e.DataAt = prev.DataAt
				e.LimitReached = prev.LimitReached
				if e.Plan == "" {
					e.Plan = prev.Plan
				}
			}
		}
	}
	t.entries = results
	t.lastRefresh = time.Now()
	for _, e := range results {
		defaultHistory.recordQuota(e, t.lastRefresh)
	}
	t.mu.Unlock()
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

func modeRank(mode string) int {
	switch mode {
	case coreauth.UsageModeLast:
		return 1
	case coreauth.UsageModeOff:
		return 2
	}
	return 0
}

func (t *Tracker) fetch(ctx context.Context, a *coreauth.Auth) *Entry {
	entry := &Entry{AuthID: a.ID, Provider: strings.ToLower(a.Provider), FetchedAt: time.Now()}
	_, entry.Email = a.AccountInfo()
	token := coreauth.AuthAccessToken(a)
	if token == "" {
		entry.Error = "sem access token"
		return entry
	}
	var err error
	switch entry.Provider {
	case "claude":
		err = t.fetchClaude(ctx, token, entry)
	case "codex":
		err = t.fetchCodex(ctx, token, codexAccountID(a), entry)
	}
	if err == nil {
		entry.DataAt = entry.FetchedAt
	} else {
		entry.Error = err.Error()
		log.WithField("provider", entry.Provider).Debugf("subsusage: fetch failed: %v", err)
	}
	return entry
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
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

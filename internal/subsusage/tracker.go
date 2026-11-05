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

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	pollInterval    = 3 * time.Minute
	minRefreshDelay = 20 * time.Second
	// staleAfter bounds how long windows from the last successful fetch are reused.
	staleAfter     = 15 * time.Minute
	requestTimeout = 20 * time.Second

	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
)

// Window is one usage window of a subscription.
type Window struct {
	Kind        string     `json:"kind"` // "short" (e.g. 5h) or "weekly"
	Label       string     `json:"label"`
	UsedPercent float64    `json:"used_percent"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
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
	// LimitReached is set when the upstream reports the subscription limit as reached.
	LimitReached bool `json:"limit_reached"`
}

func (e *Entry) weekly() *Window {
	for i := range e.Windows {
		if e.Windows[i].Kind == "weekly" {
			return &e.Windows[i]
		}
	}
	return nil
}

func (e *Entry) resetInfo() coreauth.ResetInfo {
	info := coreauth.ResetInfo{Exhausted: e.LimitReached}
	now := time.Now()
	for _, w := range e.Windows {
		// A window whose reset time has passed is no longer exhausted.
		if w.UsedPercent >= 100 && (w.ResetAt == nil || w.ResetAt.After(now)) {
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
		groups[provider] = coreauth.SortBySoonestReset(group, now)
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
			if !activeSet && !a.Disabled && !e.resetInfo().Exhausted {
				e.Active = true
				activeSet = true
			}
			out = append(out, e)
		}
	}
	return out
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
		err = t.fetchCodex(ctx, token, metaString(a, "account_id"), entry)
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

func (t *Tracker) fetchClaude(ctx context.Context, token string, entry *Entry) error {
	body, err := t.get(ctx, claudeUsageURL, map[string]string{
		"Authorization":  "Bearer " + token,
		"anthropic-beta": "oauth-2025-04-20",
		"Accept":         "application/json",
		"User-Agent":     "claude-cli/2.0.0 (external, cli)",
	})
	if err != nil {
		return err
	}
	for _, spec := range []struct{ key, kind, label string }{
		{"five_hour", "short", "5h"},
		{"seven_day", "weekly", "Semanal"},
	} {
		node := ojson.GetBytes(body, spec.key)
		if !node.Exists() || !node.IsObject() {
			continue
		}
		w := Window{Kind: spec.kind, Label: spec.label, UsedPercent: node.Get("utilization").Float()}
		if raw := strings.TrimSpace(node.Get("resets_at").String()); raw != "" {
			if ts, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
				ts = ts.UTC()
				w.ResetAt = &ts
			}
		}
		entry.Windows = append(entry.Windows, w)
	}
	if len(entry.Windows) == 0 {
		return fmt.Errorf("resposta sem janelas de uso")
	}
	return nil
}

func (t *Tracker) fetchCodex(ctx context.Context, token, accountID string, entry *Entry) error {
	headers := map[string]string{
		"Authorization": "Bearer " + token,
		"Accept":        "application/json",
		"User-Agent":    "codex_cli_rs/0.50.0",
	}
	if accountID != "" {
		headers["ChatGPT-Account-Id"] = accountID
	}
	body, err := t.get(ctx, codexUsageURL, headers)
	if err != nil {
		return err
	}
	entry.Plan = ojson.GetBytes(body, "plan_type").String()
	limitReached := ojson.GetBytes(body, "rate_limit.limit_reached").Bool()
	for _, key := range []string{"primary_window", "secondary_window"} {
		node := ojson.GetBytes(body, "rate_limit."+key)
		if !node.Exists() || !node.IsObject() {
			continue
		}
		w := Window{UsedPercent: node.Get("used_percent").Float()}
		if secs := node.Get("limit_window_seconds").Int(); secs > 0 && secs <= 24*3600 {
			w.Kind, w.Label = "short", fmt.Sprintf("%dh", secs/3600)
		} else {
			w.Kind, w.Label = "weekly", "Semanal"
		}
		if at := node.Get("reset_at").Int(); at > 0 {
			ts := time.Unix(at, 0).UTC()
			w.ResetAt = &ts
		} else if after := node.Get("reset_after_seconds").Int(); after > 0 {
			ts := time.Now().Add(time.Duration(after) * time.Second).UTC()
			w.ResetAt = &ts
		}
		entry.Windows = append(entry.Windows, w)
	}
	entry.LimitReached = limitReached
	if len(entry.Windows) == 0 {
		return fmt.Errorf("resposta sem janelas de uso")
	}
	return nil
}

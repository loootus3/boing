package subsusage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	// Not *.json: the auth watcher treats every JSON file in the auth dir as a credential.
	historyFileName    = "usage-history.dat"
	historyKeepHours   = 31 * 24
	quotaKeepDuration  = 8 * 24 * time.Hour
	quotaPointSpacing  = 10 * time.Minute
	historyFlushPeriod = 2 * time.Minute
)

// hourStat aggregates proxied requests of one credential within one hour.
type hourStat struct {
	Requests int64 `json:"req"`
	Failed   int64 `json:"fail,omitempty"`
	Input    int64 `json:"in,omitempty"`
	Output   int64 `json:"out,omitempty"`
	Cached   int64 `json:"cache,omitempty"`
}

// quotaPoint is a remaining-percent sample of a credential's windows.
type quotaPoint struct {
	T      int64    `json:"t"`
	Short  *float64 `json:"s,omitempty"`
	Weekly *float64 `json:"w,omitempty"`
}

type historyData struct {
	// Hours maps auth ID -> unix hour start -> stats.
	Hours map[string]map[int64]*hourStat `json:"hours"`
	// Quota maps auth ID -> samples, oldest first.
	Quota map[string][]quotaPoint `json:"quota"`
	// Providers maps auth ID -> provider, so removed credentials keep their provider.
	Providers map[string]string `json:"providers"`
}

// History records request activity and quota samples for the charts on /usage.
type History struct {
	mu    sync.Mutex
	path  string
	data  historyData
	dirty bool
	once  sync.Once
}

var defaultHistory = &History{data: newHistoryData()}

func newHistoryData() historyData {
	return historyData{
		Hours:     make(map[string]map[int64]*hourStat),
		Quota:     make(map[string][]quotaPoint),
		Providers: make(map[string]string),
	}
}

// DefaultHistory returns the process-wide history store.
func DefaultHistory() *History { return defaultHistory }

// Start loads the history from dir and starts the periodic flush and usage subscription.
func (h *History) Start(dir string) {
	h.once.Do(func() {
		if dir != "" {
			h.path = filepath.Join(dir, historyFileName)
			h.load()
		}
		usage.RegisterPlugin(h)
		go func() {
			for {
				time.Sleep(historyFlushPeriod)
				h.flush()
			}
		}()
	})
}

func (h *History) load() {
	raw, err := os.ReadFile(h.path)
	if err != nil {
		return
	}
	var data historyData
	if errUnmarshal := json.Unmarshal(raw, &data); errUnmarshal != nil {
		log.Warnf("subsusage: ignoring unreadable %s: %v", h.path, errUnmarshal)
		return
	}
	if data.Hours == nil {
		data.Hours = make(map[string]map[int64]*hourStat)
	}
	if data.Quota == nil {
		data.Quota = make(map[string][]quotaPoint)
	}
	if data.Providers == nil {
		data.Providers = make(map[string]string)
	}
	h.mu.Lock()
	h.data = data
	h.mu.Unlock()
}

func (h *History) flush() {
	h.mu.Lock()
	h.pruneLocked(time.Now())
	if !h.dirty || h.path == "" {
		h.mu.Unlock()
		return
	}
	raw, err := json.Marshal(h.data)
	h.dirty = false
	h.mu.Unlock()
	if err == nil {
		tmp := h.path + ".tmp"
		if err = os.WriteFile(tmp, raw, 0o600); err == nil {
			err = os.Rename(tmp, h.path)
		}
	}
	if err != nil {
		log.Warnf("subsusage: save history: %v", err)
		// Retry on the next flush.
		h.mu.Lock()
		h.dirty = true
		h.mu.Unlock()
	}
}

func (h *History) pruneLocked(now time.Time) {
	minHour := now.Add(-historyKeepHours * time.Hour).Unix()
	for id, hours := range h.data.Hours {
		for hour := range hours {
			if hour < minHour {
				delete(hours, hour)
			}
		}
		if len(hours) == 0 {
			delete(h.data.Hours, id)
		}
	}
	minQuota := now.Add(-quotaKeepDuration).Unix()
	for id, points := range h.data.Quota {
		i := sort.Search(len(points), func(i int) bool { return points[i].T >= minQuota })
		if i == len(points) {
			delete(h.data.Quota, id)
			continue
		}
		h.data.Quota[id] = append([]quotaPoint(nil), points[i:]...)
	}
	for id := range h.data.Providers {
		if _, ok := h.data.Hours[id]; ok {
			continue
		}
		if _, ok := h.data.Quota[id]; ok {
			continue
		}
		delete(h.data.Providers, id)
	}
}

// HandleUsage implements usage.Plugin: it counts Claude and Codex requests per credential.
func (h *History) HandleUsage(_ context.Context, record usage.Record) {
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	if provider != "claude" && provider != "codex" {
		return
	}
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		return
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	hour := at.Truncate(time.Hour).Unix()

	h.mu.Lock()
	defer h.mu.Unlock()
	hours := h.data.Hours[authID]
	if hours == nil {
		hours = make(map[int64]*hourStat)
		h.data.Hours[authID] = hours
	}
	stat := hours[hour]
	if stat == nil {
		stat = &hourStat{}
		hours[hour] = stat
	}
	stat.Requests++
	if record.Failed {
		stat.Failed++
	}
	stat.Input += record.Detail.InputTokens
	stat.Output += record.Detail.OutputTokens
	stat.Cached += record.Detail.CachedTokens + record.Detail.CacheReadTokens
	h.data.Providers[authID] = provider
	h.dirty = true
}

// recordQuota stores a remaining-percent sample for a freshly fetched entry.
func (h *History) recordQuota(e *Entry, now time.Time) {
	if e == nil || len(e.Windows) == 0 || e.Error != "" {
		return
	}
	point := quotaPoint{T: now.Unix()}
	for _, w := range e.Windows {
		remaining := 100 - w.UsedPercent
		if remaining < 0 {
			remaining = 0
		}
		switch w.Kind {
		case "short":
			if point.Short == nil {
				v := remaining
				point.Short = &v
			}
		case "weekly", "monthly":
			if point.Weekly == nil {
				v := remaining
				point.Weekly = &v
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	points := h.data.Quota[e.AuthID]
	if n := len(points); n > 0 && now.Unix()-points[n-1].T < int64(quotaPointSpacing/time.Second) {
		points[n-1] = point
	} else {
		points = append(points, point)
	}
	h.data.Quota[e.AuthID] = points
	h.data.Providers[e.AuthID] = e.Provider
	h.dirty = true
}

// Bucket is one time slot of the activity chart.
type Bucket struct {
	T         int64            `json:"t"`
	Requests  map[string]int64 `json:"requests"` // provider -> requests
	Tokens    map[string]int64 `json:"tokens"`   // provider -> input+output tokens
	Failed    int64            `json:"failed"`
	ByAccount map[string]int64 `json:"by_account"` // auth ID -> requests
}

// AccountTotals summarizes one credential over the selected range.
type AccountTotals struct {
	Requests int64 `json:"requests"`
	Failed   int64 `json:"failed"`
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
}

// HistoryView is the JSON payload for /usage/api/history.
type HistoryView struct {
	Range     string                   `json:"range"`
	Step      int64                    `json:"step"`
	Buckets   []Bucket                 `json:"buckets"`
	Accounts  map[string]AccountTotals `json:"accounts"`
	Providers map[string]string        `json:"providers"`
	Quota     map[string][]quotaPoint  `json:"quota"`
}

// View aggregates history for a range: "24h" (hourly), "7d" or "30d" (daily).
func (h *History) View(rangeKey string, now time.Time) HistoryView {
	var span, step time.Duration
	switch rangeKey {
	case "24h":
		span, step = 24*time.Hour, time.Hour
	case "30d":
		span, step = 30*24*time.Hour, 24*time.Hour
	default:
		rangeKey, span, step = "7d", 7*24*time.Hour, 24*time.Hour
	}
	loc := now.Location()
	slots := int(span / step)
	// Calendar arithmetic keeps day buckets on local midnights across DST changes.
	slotStart := func(i int) time.Time {
		y, m, d := now.In(loc).Date()
		if step == time.Hour {
			hr := now.In(loc).Hour()
			return time.Date(y, m, d, hr-(slots-1)+i, 0, 0, 0, loc)
		}
		return time.Date(y, m, d-(slots-1)+i, 0, 0, 0, 0, loc)
	}
	start := slotStart(0)
	end := slotStart(slots)
	view := HistoryView{
		Range:     rangeKey,
		Step:      int64(step / time.Second),
		Buckets:   make([]Bucket, slots),
		Accounts:  make(map[string]AccountTotals),
		Providers: make(map[string]string),
		Quota:     make(map[string][]quotaPoint),
	}
	for i := range view.Buckets {
		view.Buckets[i] = Bucket{
			T:         slotStart(i).Unix(),
			Requests:  map[string]int64{},
			Tokens:    map[string]int64{},
			ByAccount: map[string]int64{},
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for id, provider := range h.data.Providers {
		view.Providers[id] = provider
	}
	for id, hours := range h.data.Hours {
		provider := h.data.Providers[id]
		totals := view.Accounts[id]
		for hour, stat := range hours {
			t := time.Unix(hour, 0).In(loc)
			if t.Before(start) || !t.Before(end) {
				continue
			}
			var idx int
			if step == time.Hour {
				idx = int(t.Sub(start) / time.Hour)
			} else {
				y, m, d := t.Date()
				day := time.Date(y, m, d, 0, 0, 0, 0, loc)
				idx = int(day.Sub(start).Hours()/24 + 0.5)
			}
			if idx < 0 || idx >= slots {
				continue
			}
			b := &view.Buckets[idx]
			b.Requests[provider] += stat.Requests
			b.Tokens[provider] += stat.Input + stat.Output
			b.Failed += stat.Failed
			b.ByAccount[id] += stat.Requests
			totals.Requests += stat.Requests
			totals.Failed += stat.Failed
			totals.Input += stat.Input
			totals.Output += stat.Output
			totals.Cached += stat.Cached
		}
		view.Accounts[id] = totals
	}
	// Quota samples always cover the full retention so the page can estimate burn rates
	// independently of the selected chart range.
	minQuota := now.Add(-quotaKeepDuration).Unix()
	for id, points := range h.data.Quota {
		i := sort.Search(len(points), func(i int) bool { return points[i].T >= minQuota })
		if i < len(points) {
			view.Quota[id] = append([]quotaPoint(nil), points[i:]...)
		}
	}
	return view
}

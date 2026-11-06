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
	historyKeepHours   = 62 * 24 // two 30-day ranges, for the previous-period comparison
	quotaKeepDuration  = 8 * 24 * time.Hour
	quotaPointSpacing  = 10 * time.Minute
	historyFlushPeriod = 2 * time.Minute
	// historyVersion 2 stores fresh input, cache reads and cache writes separately; version 3
	// adds per-model token counts for cost estimates.
	historyVersion = 3
	heatDays       = 28
)

// hourStat aggregates proxied requests of one credential within one hour.
type hourStat struct {
	Requests int64 `json:"req"`
	Failed   int64 `json:"fail,omitempty"`
	// Input counts fresh input tokens, excluding anything served from or written to the cache.
	Input  int64 `json:"in,omitempty"`
	Output int64 `json:"out,omitempty"`
	// Cached counts cache read tokens.
	Cached int64 `json:"cache,omitempty"`
	// CacheWrite counts cache creation tokens.
	CacheWrite int64 `json:"cw,omitempty"`
	// Models maps pricingKey -> tokens of that model. Hours recorded before version 3 have no
	// entries; their tokens cannot be priced.
	Models map[string]*modelStat `json:"m,omitempty"`
}

// modelStat counts the tokens of one pricing key within one hour.
type modelStat struct {
	Requests  int64 `json:"req"`
	Input     int64 `json:"in,omitempty"`
	CacheRead int64 `json:"cache,omitempty"`
	// CacheWrite5m counts writes billed at the 5-minute rate, CacheWrite1h those billed at the
	// 1-hour rate (1-hour and longer TTLs).
	CacheWrite5m int64 `json:"cw5,omitempty"`
	CacheWrite1h int64 `json:"cw1h,omitempty"`
	Output       int64 `json:"out,omitempty"`
}

func (s *hourStat) tokens() int64 { return s.Input + s.Cached + s.CacheWrite + s.Output }

func (m *modelStat) tokens() int64 {
	return m.Input + m.CacheRead + m.CacheWrite5m + m.CacheWrite1h + m.Output
}

// splitCacheWrite divides cache writes by billing TTL. Writes without a TTL breakdown are
// billed at the 5-minute rate, the API default.
func splitCacheWrite(d usage.Detail, write int64) (w5m, w1h int64) {
	for _, p := range []*int64{d.CacheCreation1hTokens, d.CacheCreation12hTokens} {
		if p != nil && *p > 0 {
			w1h += *p
		}
	}
	w1h = min(w1h, write)
	return write - w1h, w1h
}

// normalizeTokens splits a usage record into fresh input, cache read, cache write and output
// tokens. Anthropic reports input_tokens without cache, while OpenAI-style input includes the
// cached subset. The Claude parser sets CachedTokens to the cache creation count when there is
// no cache read, so for Claude only CacheReadTokens is a read count. Data recorded before v2
// is approximate for such requests: its cache total may include cache writes.
func normalizeTokens(provider string, d usage.Detail) (fresh, read, write, output int64) {
	output = max(d.OutputTokens, 0)
	if provider == "codex" {
		read = max(d.CachedTokens, d.CacheReadTokens, 0)
		return max(0, d.InputTokens-read), read, 0, output
	}
	return max(d.InputTokens, 0), max(d.CacheReadTokens, 0), max(d.CacheCreationTokens, 0), output
}

// migrateHistory upgrades data loaded from an older file and reports whether it changed.
func migrateHistory(data *historyData) bool {
	if data.Version >= historyVersion {
		return false
	}
	if data.Version >= 2 {
		// Version 3 only adds per-model counts.
		data.Version = historyVersion
		return true
	}
	// Before v2 both CachedTokens and CacheReadTokens were added, counting cache reads twice.
	// Codex input also included the cached subset.
	for id, hours := range data.Hours {
		provider := data.Providers[id]
		if provider != "claude" && provider != "codex" {
			continue
		}
		for _, stat := range hours {
			stat.Cached /= 2
			if provider == "codex" {
				stat.Input = max(0, stat.Input-stat.Cached)
			}
		}
	}
	data.Version = historyVersion
	return true
}

// quotaPoint is a remaining-percent sample of a credential's windows.
type quotaPoint struct {
	T      int64    `json:"t"`
	Short  *float64 `json:"s,omitempty"`
	Weekly *float64 `json:"w,omitempty"`
}

type historyData struct {
	// Version is the file format version; see historyVersion.
	Version int `json:"v"`
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
	// lastUsed maps auth ID -> time of the last proxied request. It is not persisted.
	lastUsed map[string]time.Time
}

var defaultHistory = &History{data: newHistoryData()}

func newHistoryData() historyData {
	return historyData{
		Version:   historyVersion,
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
	migrated := migrateHistory(&data)
	h.mu.Lock()
	h.data = data
	h.dirty = h.dirty || migrated
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
	fresh, read, write, output := normalizeTokens(provider, record.Detail)
	stat.Input += fresh
	stat.Output += output
	stat.Cached += read
	stat.CacheWrite += write
	model := record.ResponseModel
	if strings.TrimSpace(model) == "" {
		model = record.Model
	}
	key := pricingKey(model, record.Detail.Speed, fresh+read+write)
	if stat.Models == nil {
		stat.Models = make(map[string]*modelStat)
	}
	ms := stat.Models[key]
	if ms == nil {
		ms = &modelStat{}
		stat.Models[key] = ms
	}
	w5m, w1h := splitCacheWrite(record.Detail, write)
	ms.Requests++
	ms.Input += fresh
	ms.CacheRead += read
	ms.CacheWrite5m += w5m
	ms.CacheWrite1h += w1h
	ms.Output += output
	h.data.Providers[authID] = provider
	if h.lastUsed == nil {
		h.lastUsed = make(map[string]time.Time)
	}
	if at.After(h.lastUsed[authID]) {
		h.lastUsed[authID] = at
	}
	h.dirty = true
}

// LastUsed returns the time of the last proxied request of a credential since the process
// started, or the zero time if there was none.
func (h *History) LastUsed(authID string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastUsed[authID]
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
	// The last point is provisional: it is replaced until it lies quotaPointSpacing after the
	// point before it, then kept. Comparing the new sample with the last point instead would
	// keep sliding that point forward for an account polled more often than the spacing, so it
	// would never gain new points.
	// A minute of tolerance keeps polls at about the spacing (with jitter) appending every time,
	// and a provisional point older than the spacing is kept, so a gap in polling never erases it.
	spacing := int64(quotaPointSpacing / time.Second)
	if n := len(points); n > 1 && points[n-1].T-points[n-2].T < spacing-60 && now.Unix()-points[n-1].T < spacing {
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
	Tokens    map[string]int64 `json:"tokens"`   // provider -> all processed tokens
	Failed    int64            `json:"failed"`
	ByAccount map[string]int64 `json:"by_account"` // auth ID -> requests
	// TokensByAccount maps auth ID -> all processed tokens (fresh input, cache read and write, output).
	TokensByAccount map[string]int64 `json:"tokens_by_account"`
	// Cost maps provider -> estimated API cost in USD; CostByAccount maps auth ID -> USD.
	Cost          map[string]float64 `json:"cost"`
	CostByAccount map[string]float64 `json:"cost_by_account"`
	// CostByModel, TokensByModel and RequestsByModel are keyed by pricing key (Claude only).
	// Priced models only contribute to CostByModel.
	CostByModel     map[string]float64 `json:"cost_by_model"`
	TokensByModel   map[string]int64   `json:"tokens_by_model"`
	RequestsByModel map[string]int64   `json:"requests_by_model"`
}

// AccountTotals summarizes one credential over the selected range.
type AccountTotals struct {
	Requests int64 `json:"requests"`
	Failed   int64 `json:"failed"`
	Input    int64 `json:"input"` // fresh input, excluding cache
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"` // cache reads
	// CacheWrite counts cache creation tokens.
	CacheWrite int64 `json:"cache_write"`
	// Cost estimates the API price of the priced tokens; UnpricedTokens counts tokens of
	// unknown models, which Cost leaves out. Claude tokens recorded before per-model tracking
	// are priced as legacyModelKey and included in Cost.
	Cost           CostBreakdown `json:"cost"`
	UnpricedTokens int64         `json:"unpriced_tokens"`
}

// legacyModelKey groups Claude tokens recorded before per-model tracking (history v3). They
// are priced as legacyAssumedModel with cache writes at the 1-hour rate, the Claude Code
// default, so ranges that start before tracking still get a cost estimate.
const (
	legacyModelKey     = "legacy"
	legacyAssumedModel = "claude-opus-5-5"
)

type modelCost struct {
	stat   *modelStat
	cost   CostBreakdown
	priced bool
}

// legacyStat returns the part of a Claude hour not covered by per-model counts.
func legacyStat(stat *hourStat) *modelStat {
	var m modelStat
	for _, ms := range stat.Models {
		m.Requests += ms.Requests
		m.Input += ms.Input
		m.CacheRead += ms.CacheRead
		m.CacheWrite1h += ms.CacheWrite5m + ms.CacheWrite1h
		m.Output += ms.Output
	}
	rest := &modelStat{
		Requests:     max(0, stat.Requests-m.Requests),
		Input:        max(0, stat.Input-m.Input),
		CacheRead:    max(0, stat.Cached-m.CacheRead),
		CacheWrite1h: max(0, stat.CacheWrite-m.CacheWrite1h),
		Output:       max(0, stat.Output-m.Output),
	}
	if rest.tokens() == 0 {
		return nil
	}
	return rest
}

// addHour adds one hour of a credential to the totals and returns the cost of each of its
// pricing keys, including legacyModelKey for untracked Claude tokens.
func (t *AccountTotals) addHour(stat *hourStat, provider string) map[string]modelCost {
	t.Requests += stat.Requests
	t.Failed += stat.Failed
	t.Input += stat.Input
	t.Output += stat.Output
	t.Cached += stat.Cached
	t.CacheWrite += stat.CacheWrite
	costs := make(map[string]modelCost, len(stat.Models))
	var tracked int64
	for key, ms := range stat.Models {
		tracked += ms.tokens()
		cost, priced := priceOf(key, ms)
		if priced {
			t.Cost.add(cost)
		} else {
			t.UnpricedTokens += ms.tokens()
		}
		costs[key] = modelCost{ms, cost, priced}
	}
	if provider != "claude" {
		t.UnpricedTokens += max(0, stat.tokens()-tracked)
		return costs
	}
	if rest := legacyStat(stat); rest != nil {
		cost, _ := priceOf(legacyAssumedModel, rest)
		t.Cost.add(cost)
		costs[legacyModelKey] = modelCost{rest, cost, true}
	}
	return costs
}

// ModelTotals summarizes one pricing key (model, plus "+fast" / "+long") over the range.
type ModelTotals struct {
	Requests   int64         `json:"requests"`
	Input      int64         `json:"input"`
	CacheRead  int64         `json:"cache_read"`
	CacheWrite int64         `json:"cache_write"`
	Output     int64         `json:"output"`
	Priced     bool          `json:"priced"`
	Cost       CostBreakdown `json:"cost"`
}

// HistoryView is the JSON payload for /usage/api/history.
type HistoryView struct {
	Range     string                   `json:"range"`
	Step      int64                    `json:"step"`
	Buckets   []Bucket                 `json:"buckets"`
	Accounts  map[string]AccountTotals `json:"accounts"`
	Providers map[string]string        `json:"providers"`
	Quota     map[string][]quotaPoint  `json:"quota"`
	// Models aggregates Claude tokens and cost by pricing key over the range.
	Models map[string]ModelTotals `json:"models"`
	// Previous holds Claude account totals for the equally long period right before the range.
	// PreviousComplete reports whether history reaches back to its start; when false the
	// previous totals cover only part of it and should not be compared.
	Previous         map[string]AccountTotals `json:"previous"`
	PreviousComplete bool                     `json:"previous_complete"`
	// HeatTokens and HeatRequests are weekday (0 = Sunday) x hour totals in the location of the
	// View time over the last 28 days (Claude only), independent of the range. HeatDays is how
	// many of those days have data and HeatWeekdayDays how many of them fall on each weekday.
	HeatTokens      [7][24]int64 `json:"heat_tokens"`
	HeatRequests    [7][24]int64 `json:"heat_requests"`
	HeatDays        int          `json:"heat_days"`
	HeatWeekdayDays [7]int       `json:"heat_weekday_days"`
}

// View aggregates history for a range: "24h" (hourly), "7d" or "30d" (daily). Day buckets and
// heat bins use the location of now.
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
		Models:    make(map[string]ModelTotals),
		Previous:  make(map[string]AccountTotals),
	}
	for i := range view.Buckets {
		view.Buckets[i] = Bucket{
			T:         slotStart(i).Unix(),
			Requests:  map[string]int64{},
			Tokens:    map[string]int64{},
			ByAccount: map[string]int64{},

			TokensByAccount: map[string]int64{},
			Cost:            map[string]float64{},
			CostByAccount:   map[string]float64{},
			CostByModel:     map[string]float64{},
			TokensByModel:   map[string]int64{},
			RequestsByModel: map[string]int64{},
		}
	}

	heatStart := now.Add(-heatDays * 24 * time.Hour)
	var heatFirst time.Time
	haveHeat := false

	h.mu.Lock()
	defer h.mu.Unlock()
	for id, provider := range h.data.Providers {
		view.Providers[id] = provider
	}
	prevStart := slotStart(-slots)
	earliest := int64(0)
	for id, hours := range h.data.Hours {
		provider := h.data.Providers[id]
		totals := view.Accounts[id]
		prev := view.Previous[id]
		for hour, stat := range hours {
			if provider == "claude" && (earliest == 0 || hour < earliest) {
				earliest = hour
			}
			t := time.Unix(hour, 0).In(loc)
			if provider == "claude" && !t.Before(heatStart) && !t.After(now) {
				total := stat.tokens()
				view.HeatTokens[t.Weekday()][t.Hour()] += total
				view.HeatRequests[t.Weekday()][t.Hour()] += stat.Requests
				if !haveHeat || t.Before(heatFirst) {
					heatFirst, haveHeat = t, true
				}
			}
			if provider == "claude" && !t.Before(prevStart) && t.Before(start) {
				prev.addHour(stat, provider)
				continue
			}
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
			total := stat.tokens()
			b.Requests[provider] += stat.Requests
			b.Tokens[provider] += total
			b.TokensByAccount[id] += total
			b.Failed += stat.Failed
			b.ByAccount[id] += stat.Requests
			for key, mc := range totals.addHour(stat, provider) {
				if mc.priced {
					b.Cost[provider] += mc.cost.Total
					b.CostByAccount[id] += mc.cost.Total
				}
				if provider != "claude" {
					continue
				}
				ms := mc.stat
				b.CostByModel[key] += mc.cost.Total
				b.TokensByModel[key] += ms.tokens()
				b.RequestsByModel[key] += ms.Requests
				mt := view.Models[key]
				mt.Requests += ms.Requests
				mt.Input += ms.Input
				mt.CacheRead += ms.CacheRead
				mt.CacheWrite += ms.CacheWrite5m + ms.CacheWrite1h
				mt.Output += ms.Output
				mt.Priced = mc.priced
				mt.Cost.add(mc.cost)
				view.Models[key] = mt
			}
		}
		view.Accounts[id] = totals
		if provider == "claude" && prev.Requests > 0 {
			view.Previous[id] = prev
		}
	}
	view.PreviousComplete = earliest != 0 && earliest <= prevStart.Unix()
	if haveHeat {
		view.HeatDays = min(heatDays, max(1, int(now.Sub(heatFirst)/(24*time.Hour))+1))
		y, m, d := now.In(loc).Date()
		for k := 0; k < view.HeatDays; k++ {
			view.HeatWeekdayDays[time.Date(y, m, d-k, 12, 0, 0, 0, loc).Weekday()]++
		}
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

package subsusage

import (
	"math"
	"time"
)

// forecastHorizons are the hours ahead each forecast snapshot predicts.
var forecastHorizons = [4]int{3, 6, 12, 24}

const forecastKeep = 8 * 24 * time.Hour

// forecastSnap is what the page predicted for one account at the start of one hour: the
// remaining % of the weekly window (W) and of Fable's own allowance (F) forecastHorizons hours
// later. A nil value means no forecast for that horizon.
type forecastSnap struct {
	W [4]*float64 `json:"w"`
	F [4]*float64 `json:"f,omitempty"`
}

// ForecastPost is the body of POST /usage/api/forecast: account ID -> predictions.
type ForecastPost struct {
	Accounts map[string]forecastSnap `json:"accounts"`
}

func cleanPct(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	c := math.Round(math.Min(100, math.Max(0, *v))*10) / 10
	return &c
}

// RecordForecast stores the page's forecast for the current hour; per account, the first one
// posted in an hour wins, so repeated posts and several open tabs do not overwrite the baseline
// while an account missing from an earlier post can still be added. Only tracked Claude accounts
// are kept. It reports whether anything was stored.
func (h *History) RecordForecast(p ForecastPost, now time.Time) bool {
	hour := now.Truncate(time.Hour).Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	have := h.data.Forecasts[hour]
	snaps := make(map[string]*forecastSnap)
	for id, s := range p.Accounts {
		if h.data.Providers[id] != "claude" || have[id] != nil {
			continue
		}
		c := &forecastSnap{}
		any := false
		for i := range forecastHorizons {
			c.W[i], c.F[i] = cleanPct(s.W[i]), cleanPct(s.F[i])
			any = any || c.W[i] != nil || c.F[i] != nil
		}
		if any {
			snaps[id] = c
		}
	}
	if len(snaps) == 0 {
		return false
	}
	if h.data.Forecasts == nil {
		h.data.Forecasts = make(map[int64]map[string]*forecastSnap)
	}
	if have == nil {
		have = make(map[string]*forecastSnap, len(snaps))
		h.data.Forecasts[hour] = have
	}
	for id, s := range snaps {
		have[id] = s
	}
	cut := now.Add(-forecastKeep).Unix()
	for t := range h.data.Forecasts {
		if t < cut {
			delete(h.data.Forecasts, t)
		}
	}
	h.dirty = true
	return true
}

// forecastsView copies the stored snapshots; h.mu must be held.
func (h *History) forecastsView() map[int64]map[string]*forecastSnap {
	out := make(map[int64]map[string]*forecastSnap, len(h.data.Forecasts))
	for t, m := range h.data.Forecasts {
		c := make(map[string]*forecastSnap, len(m))
		for id, s := range m {
			cp := *s
			c[id] = &cp
		}
		out[t] = c
	}
	return out
}

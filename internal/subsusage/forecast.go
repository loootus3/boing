package subsusage

import (
	"math"
	"time"
)

// forecastHorizons are the hours ahead each forecast snapshot predicts.
var forecastHorizons = [4]int{3, 6, 12, 24}

// Kept for a month so the long-range chart can compare old forecasts with what happened.
const forecastKeep = 31 * 24 * time.Hour

// forecastSnap is what the page predicted for one account at the start of one hour: the
// remaining % of the weekly window (W) and of Fable's own allowance (F) forecastHorizons hours
// later. A nil value means no forecast for that horizon.
type forecastSnap struct {
	W [4]*float64 `json:"w"`
	F [4]*float64 `json:"f,omitempty"`
}

// ForecastPost is the body of POST /usage/api/forecast: account ID -> predictions, plus the
// whole projection the page drew (optional).
type ForecastPost struct {
	Accounts map[string]forecastSnap `json:"accounts"`
	Curve    *CurvePost              `json:"curve,omitempty"`
}

// CurvePost is a projection as the page computed it: per account the weekly remaining % every
// Step seconds from the post time on, and the pace (%/h, all accounts) it assumed.
type CurvePost struct {
	Step int                  `json:"step"`
	Pace float64              `json:"pace"`
	Accs map[string][]float64 `json:"accs"`
}

// curveSnap is a stored projection: the values are whole percents, the first one at At (when it
// was posted, within the hour the snapshot is keyed by).
type curveSnap struct {
	At   int64              `json:"at"`
	Step int                `json:"step"`
	Pace float64            `json:"pace,omitempty"`
	Accs map[string][]int16 `json:"accs"`
}

// Curve limits: steps of 30 min to 6 h, at most 15 days of points.
const (
	curveMinStep = 1800
	curveMaxStep = 6 * 3600
	curveMaxSpan = 15 * 24 * 3600
	// curveDenseKeep: every hourly projection is kept this long; older ones only every 6th hour.
	curveDenseKeep = 72 * time.Hour
)

// cleanCurve validates a posted curve; nil when unusable. Only tracked Claude accounts are kept.
func (h *History) cleanCurve(c *CurvePost) *curveSnap {
	if c == nil || c.Step < curveMinStep || c.Step > curveMaxStep || len(c.Accs) == 0 || len(c.Accs) > 32 {
		return nil
	}
	maxN := curveMaxSpan/c.Step + 1
	out := &curveSnap{Step: c.Step, Accs: make(map[string][]int16, len(c.Accs))}
	if !math.IsNaN(c.Pace) && !math.IsInf(c.Pace, 0) && c.Pace > 0 {
		out.Pace = math.Round(math.Min(c.Pace, 1000)*100) / 100
	}
	for id, vs := range c.Accs {
		if h.data.Providers[id] != "claude" || len(vs) < 2 || len(vs) > maxN {
			continue
		}
		row := make([]int16, len(vs))
		for i, v := range vs {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil
			}
			row[i] = int16(math.Round(math.Min(100, math.Max(0, v))))
		}
		out.Accs[id] = row
	}
	if len(out.Accs) == 0 {
		return nil
	}
	return out
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
	stored := h.recordCurveLocked(p.Curve, hour, now)
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
		return stored
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

// recordCurveLocked stores the hour's projection (the first one posted wins) and thins out the
// old ones: hourly for curveDenseKeep, then every 6th hour, until forecastKeep. h.mu must be held.
func (h *History) recordCurveLocked(c *CurvePost, hour int64, now time.Time) bool {
	if h.data.Curves[hour] != nil {
		return false
	}
	snap := h.cleanCurve(c)
	if snap == nil {
		return false
	}
	if h.data.Curves == nil {
		h.data.Curves = make(map[int64]*curveSnap)
	}
	snap.At = now.Unix()
	h.data.Curves[hour] = snap
	dense, cut := now.Add(-curveDenseKeep).Unix(), now.Add(-forecastKeep).Unix()
	for t := range h.data.Curves {
		if t < cut || (t < dense && (t/3600)%6 != 0) {
			delete(h.data.Curves, t)
		}
	}
	h.dirty = true
	return true
}

// Curves copies the stored projections (unix hour -> projection).
func (h *History) Curves() map[int64]*curveSnap {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[int64]*curveSnap, len(h.data.Curves))
	for t, c := range h.data.Curves {
		cp := &curveSnap{At: c.At, Step: c.Step, Pace: c.Pace, Accs: make(map[string][]int16, len(c.Accs))}
		for id, row := range c.Accs {
			cp.Accs[id] = append([]int16(nil), row...)
		}
		out[t] = cp
	}
	return out
}

// forecastsView copies the stored snapshots of hours in [from, to); h.mu must be held.
func (h *History) forecastsView(from, to int64) map[int64]map[string]*forecastSnap {
	out := make(map[int64]map[string]*forecastSnap)
	for t, m := range h.data.Forecasts {
		if t < from || t >= to {
			continue
		}
		c := make(map[string]*forecastSnap, len(m))
		for id, s := range m {
			cp := *s
			c[id] = &cp
		}
		out[t] = c
	}
	return out
}

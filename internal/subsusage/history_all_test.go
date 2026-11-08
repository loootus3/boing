package subsusage

import (
	"testing"
	"time"
)

func addHour(h *History, id, provider string, hour int64, req int64) {
	if h.data.Hours[id] == nil {
		h.data.Hours[id] = map[int64]*hourStat{}
	}
	h.data.Hours[id][hour] = &hourStat{Requests: req, Input: req}
	h.data.Providers[id] = provider
}

func TestPruneKeepsOldHours(t *testing.T) {
	h := &History{data: newHistoryData()}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour).Truncate(time.Hour).Unix()
	addHour(h, "c", "claude", old, 3)
	h.pruneLocked(now)
	if h.data.Hours["c"][old] == nil || h.data.Providers["c"] != "claude" {
		t.Fatal("old hour must be kept")
	}
}

func TestQuotaCompaction(t *testing.T) {
	h := &History{data: newHistoryData()}
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	base := now.Add(-20 * 24 * time.Hour).Truncate(time.Hour).Unix()
	var pts []quotaPoint
	for i := int64(0); i < 12; i++ { // two hours of 10-minute samples... six per hour
		pts = append(pts, quotaPoint{T: base + i*600})
	}
	recentStart := now.Add(-2 * 24 * time.Hour).Unix()
	for i := int64(0); i < 5; i++ {
		pts = append(pts, quotaPoint{T: recentStart + i*600})
	}
	h.data.Quota["a"] = pts
	h.pruneLocked(now)
	got := h.data.Quota["a"]
	if len(got) != 2+5 {
		t.Fatalf("len = %d: %+v", len(got), got)
	}
	if got[0].T != base+5*600 || got[1].T != base+11*600 {
		t.Fatalf("not last-per-hour: %+v", got[:2])
	}
	if !h.dirty {
		t.Fatal("compaction must set dirty")
	}
	h.dirty = false
	h.pruneLocked(now)
	if h.dirty || len(h.data.Quota["a"]) != 7 {
		t.Fatalf("not idempotent: dirty=%v len=%d", h.dirty, len(h.data.Quota["a"]))
	}
}

func TestViewAll(t *testing.T) {
	h := &History{data: newHistoryData()}
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	first := time.Date(2026, 6, 1, 5, 0, 0, 0, time.UTC).Unix()
	addHour(h, "c", "claude", first, 2)
	addHour(h, "x", "codex", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Unix(), 1)
	v := h.View("all", now)
	if v.Range != "all" || len(v.Buckets) != 10 || v.Step != 86400 {
		t.Fatalf("range=%s buckets=%d step=%d", v.Range, len(v.Buckets), v.Step)
	}
	if v.Buckets[0].T != time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Unix() || v.Buckets[0].Requests["claude"] != 2 {
		t.Fatalf("first bucket = %+v", v.Buckets[0])
	}
	if len(v.Previous) != 0 || v.PreviousComplete {
		t.Fatalf("previous = %+v complete=%v", v.Previous, v.PreviousComplete)
	}
	empty := (&History{data: newHistoryData()}).View("all", now)
	if len(empty.Buckets) != 7 {
		t.Fatalf("empty buckets = %d", len(empty.Buckets))
	}
}

func TestViewWithHeatAll(t *testing.T) {
	h := &History{data: newHistoryData()}
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	old := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	addHour(h, "c", "claude", old.Unix(), 4)
	v := h.ViewWith("7d", "all", now)
	if v.HeatRange != "all" || v.HeatRequests[old.Weekday()][10] != 4 || v.HeatDays <= 28 {
		t.Fatalf("heat all: range=%s days=%d req=%d", v.HeatRange, v.HeatDays, v.HeatRequests[old.Weekday()][10])
	}
	sum := 0
	for _, n := range v.HeatWeekdayDays {
		sum += n
	}
	if sum != v.HeatDays {
		t.Fatalf("weekday days %d != %d", sum, v.HeatDays)
	}
	n := h.View("7d", now)
	if n.HeatRange != "28d" || n.HeatRequests[old.Weekday()][10] != 0 {
		t.Fatalf("28d heat = %+v", n.HeatRange)
	}
}

func TestQuotaAll(t *testing.T) {
	h := &History{data: newHistoryData()}
	h.data.Quota["a"] = []quotaPoint{{T: 1000}, {T: 4000}, {T: 4100}}
	got := h.QuotaAll("a")
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	got[0].T = 1
	if h.data.Quota["a"][0].T != 1000 {
		t.Fatal("must be a copy")
	}
	if u := h.QuotaAll("nope"); u == nil || len(u) != 0 {
		t.Fatalf("unknown = %#v", u)
	}
}

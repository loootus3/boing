package subsusage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestHistoryAggregates(t *testing.T) {
	h := &History{data: newHistoryData()}
	now := time.Now()
	for _, p := range []string{"claude", "claude", "codex", "gemini"} {
		h.HandleUsage(context.Background(), usage.Record{
			Provider: p, AuthID: p + "-a", RequestedAt: now,
			Detail: usage.Detail{InputTokens: 10, OutputTokens: 5},
		})
	}
	v := h.View("24h", now)
	if len(v.Buckets) != 24 {
		t.Fatalf("buckets = %d", len(v.Buckets))
	}
	last := v.Buckets[len(v.Buckets)-1]
	if last.Requests["claude"] != 2 || last.Requests["codex"] != 1 || last.Tokens["claude"] != 30 || last.TokensByAccount["claude-a"] != 30 {
		t.Fatalf("last bucket = %+v", last)
	}
	if _, ok := v.Accounts["gemini-a"]; ok {
		t.Fatal("non Claude/Codex providers must be ignored")
	}
	if d := h.View("7d", now); d.Buckets[len(d.Buckets)-1].Requests["claude"] != 2 {
		t.Fatalf("7d last bucket = %+v", d.Buckets[len(d.Buckets)-1])
	}

	w := 40.0
	h.recordQuota(&Entry{AuthID: "claude-a", Provider: "claude", Windows: []Window{{Kind: "weekly", UsedPercent: 60}}}, now)
	if q := h.View("7d", now).Quota["claude-a"]; len(q) != 1 || *q[0].Weekly != w {
		t.Fatalf("quota = %+v", q)
	}
}

func TestNormalizeTokens(t *testing.T) {
	claude := usage.Detail{InputTokens: 10, OutputTokens: 5, CachedTokens: 80, CacheReadTokens: 80, CacheCreationTokens: 7}
	fresh, read, write, out := normalizeTokens("claude", claude)
	if fresh != 10 || read != 80 || write != 7 || out != 5 {
		t.Fatalf("claude = %d %d %d %d", fresh, read, write, out)
	}
	// The Claude parser copies the cache creation count into CachedTokens when nothing was read.
	writeOnly := usage.Detail{InputTokens: 3, OutputTokens: 5, CachedTokens: 40, CacheCreationTokens: 40}
	fresh, read, write, out = normalizeTokens("claude", writeOnly)
	if fresh != 3 || read != 0 || write != 40 || out != 5 {
		t.Fatalf("claude cache write only = %d %d %d %d", fresh, read, write, out)
	}
	codex := usage.Detail{InputTokens: 100, OutputTokens: 5, CachedTokens: 60, CacheReadTokens: 60}
	fresh, read, write, out = normalizeTokens("codex", codex)
	if fresh != 40 || read != 60 || write != 0 || out != 5 {
		t.Fatalf("codex = %d %d %d %d", fresh, read, write, out)
	}
	fresh, _, _, _ = normalizeTokens("codex", usage.Detail{InputTokens: 10, CachedTokens: 30})
	if fresh != 0 {
		t.Fatalf("codex fresh must not go negative, got %d", fresh)
	}
}

func TestHistoryCacheAccounting(t *testing.T) {
	h := &History{data: newHistoryData()}
	now := time.Now()
	h.HandleUsage(context.Background(), usage.Record{
		Provider: "claude", AuthID: "c1", RequestedAt: now,
		Detail: usage.Detail{InputTokens: 10, OutputTokens: 5, CachedTokens: 80, CacheReadTokens: 80, CacheCreationTokens: 20},
	})
	v := h.View("24h", now)
	acc := v.Accounts["c1"]
	if acc.Input != 10 || acc.Cached != 80 || acc.CacheWrite != 20 || acc.Output != 5 {
		t.Fatalf("totals = %+v", acc)
	}
	if got := v.Buckets[len(v.Buckets)-1].Tokens["claude"]; got != 115 {
		t.Fatalf("bucket tokens = %d", got)
	}
	h.HandleUsage(context.Background(), usage.Record{
		Provider: "codex", AuthID: "x1", RequestedAt: now,
		Detail: usage.Detail{InputTokens: 1000, OutputTokens: 500},
	})
	v = h.View("24h", now)
	var tokens, reqs int64
	for d := range v.HeatTokens {
		for hr := range v.HeatTokens[d] {
			tokens += v.HeatTokens[d][hr]
			reqs += v.HeatRequests[d][hr]
		}
	}
	if tokens != 115 || reqs != 1 || v.HeatDays != 1 {
		t.Fatalf("heat tokens=%d reqs=%d days=%d", tokens, reqs, v.HeatDays)
	}
	local := now.Truncate(time.Hour).In(now.Location())
	if v.HeatTokens[local.Weekday()][local.Hour()] != 115 {
		t.Fatalf("heat cell missing for %v", local)
	}
}

func TestHistoryMigratesV1(t *testing.T) {
	dir := t.TempDir()
	hour := time.Now().Truncate(time.Hour).Unix()
	old := fmt.Sprintf(`{"hours":{"c1":{"%[1]d":{"req":2,"in":10,"out":4,"cache":160}},"x1":{"%[1]d":{"req":1,"in":100,"cache":120}}},`+
		`"quota":{},"providers":{"c1":"claude","x1":"codex"}}`, hour)
	if err := os.WriteFile(filepath.Join(dir, historyFileName), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &History{data: newHistoryData(), path: filepath.Join(dir, historyFileName)}
	h.load()
	if got := h.data.Hours["c1"][hour]; got.Cached != 80 || got.Input != 10 {
		t.Fatalf("claude migrated = %+v", got)
	}
	if got := h.data.Hours["x1"][hour]; got.Cached != 60 || got.Input != 40 {
		t.Fatalf("codex migrated = %+v", got)
	}
	if h.data.Version != historyVersion || !h.dirty {
		t.Fatalf("version = %d dirty = %v", h.data.Version, h.dirty)
	}
	h.flush()
	raw, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved historyData
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Version != historyVersion {
		t.Fatalf("saved version = %d err = %v", saved.Version, err)
	}
	// A second load must not halve again.
	h2 := &History{data: newHistoryData(), path: h.path}
	h2.load()
	if got := h2.data.Hours["c1"][hour]; got.Cached != 80 {
		t.Fatalf("migration repeated: %+v", got)
	}
}

func TestHistoryViewTimezone(t *testing.T) {
	zone := time.FixedZone("UTC+10", 10*3600)
	// 2026-03-01 is a Sunday. 20:30 UTC is Monday 06:30 in UTC+10.
	at := time.Date(2026, 3, 1, 20, 30, 0, 0, time.UTC)
	h := &History{data: newHistoryData()}
	h.HandleUsage(context.Background(), usage.Record{
		Provider: "claude", AuthID: "c1", RequestedAt: at,
		Detail: usage.Detail{InputTokens: 10, OutputTokens: 5},
	})
	now := at.Add(time.Hour)
	utc := h.View("7d", now.UTC())
	if utc.HeatTokens[time.Sunday][20] != 15 {
		t.Fatalf("utc heat cell missing: %v", utc.HeatTokens[time.Sunday])
	}
	v := h.View("7d", now.In(zone))
	if v.HeatTokens[time.Monday][6] != 15 || v.HeatTokens[time.Sunday][20] != 0 {
		t.Fatalf("zone heat bins wrong: mon=%v", v.HeatTokens[time.Monday])
	}
	last := v.Buckets[len(v.Buckets)-1]
	if last.Requests["claude"] != 1 || time.Unix(last.T, 0).In(zone).Hour() != 0 {
		t.Fatalf("zone day bucket = %+v", last)
	}
	if v.HeatDays != 1 || v.HeatWeekdayDays[time.Monday] != 1 {
		t.Fatalf("heat days = %d weekday days = %v", v.HeatDays, v.HeatWeekdayDays)
	}
}

func TestQueryLocation(t *testing.T) {
	if got := queryLocation("America/Sao_Paulo"); got.String() != "America/Sao_Paulo" {
		t.Fatalf("zone = %v", got)
	}
	for _, bad := range []string{"", "Nope/Zone", "../etc/passwd"} {
		if queryLocation(bad) != time.Local {
			t.Fatalf("%q must fall back to local", bad)
		}
	}
}

func TestRecordQuotaKeepsHistoryForFrequentPolls(t *testing.T) {
	h := &History{data: newHistoryData()}
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	// Polled every 2 minutes for an hour, losing 1% per poll.
	for i := 0; i <= 30; i++ {
		e := &Entry{AuthID: "a", Provider: "claude", Windows: []Window{{Kind: "weekly", UsedPercent: float64(i)}}}
		h.recordQuota(e, start.Add(time.Duration(i)*2*time.Minute))
	}
	pts := h.data.Quota["a"]
	if len(pts) < 6 || len(pts) > 8 {
		t.Fatalf("got %d points, want about one per 10 minutes", len(pts))
	}
	for i := 2; i < len(pts); i++ {
		if gap := pts[i-1].T - pts[i-2].T; gap < int64(quotaPointSpacing/time.Second) {
			t.Errorf("points %d-%d only %ds apart", i-2, i-1, gap)
		}
	}
	last := pts[len(pts)-1]
	if last.T != start.Add(60*time.Minute).Unix() || *last.Weekly != 70 {
		t.Errorf("last point = %d / %v, want the latest sample", last.T, *last.Weekly)
	}
}

func TestRecordQuotaGapsAndSlowPolls(t *testing.T) {
	h := &History{data: newHistoryData()}
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rec := func(at time.Time, used float64) {
		h.recordQuota(&Entry{AuthID: "a", Provider: "claude", Windows: []Window{{Kind: "weekly", UsedPercent: used}}}, at)
	}
	// Slow polling every ~10 min with jitter appends every sample.
	for i, sec := range []int{0, 599, 1201, 1799, 2400} {
		rec(start.Add(time.Duration(sec)*time.Second), float64(i))
	}
	if n := len(h.data.Quota["a"]); n != 5 {
		t.Fatalf("slow polling kept %d points, want 5", n)
	}
	// A provisional point followed by a long gap is kept.
	rec(start.Add(2520*time.Second), 5) // 2 min after the last: provisional
	rec(start.Add(5*time.Hour), 6)
	pts := h.data.Quota["a"]
	if got := pts[len(pts)-2].T; got != start.Add(2520*time.Second).Unix() {
		t.Errorf("point before the gap = %d, want the provisional point kept", got)
	}
}

func TestMarkProxied(t *testing.T) {
	base := int64(1_800_000_000) / 3600 * 3600
	hours := map[int64]*hourStat{base: {Requests: 3}, base + 7200: {}}
	pts := []quotaPoint{{T: base - 1200}, {T: base + 600}, {T: base + 3600 + 1200}, {T: base + 3600 + 2400}, {T: base + 7200 + 600}}
	got := markProxied(pts, hours)
	want := []bool{false, true, true, false, false}
	for k, p := range got {
		if p.Proxy != want[k] {
			t.Errorf("point %d proxy = %v, want %v", k, p.Proxy, want[k])
		}
	}
}

func TestRecordQuotaKeepsFableRemaining(t *testing.T) {
	h := &History{data: historyData{Quota: map[string][]quotaPoint{}, Providers: map[string]string{}}}
	used := 60.0
	h.recordQuota(&Entry{AuthID: "a", Provider: "claude", Windows: []Window{{Kind: "weekly", UsedPercent: 44, FableLimitPercent: &used}}}, time.Unix(1000, 0))
	p := h.data.Quota["a"]
	if len(p) != 1 || p[0].Fable == nil || *p[0].Fable != 40 || *p[0].Weekly != 56 {
		t.Fatalf("got %+v", p)
	}
}

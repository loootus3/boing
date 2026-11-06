package subsusage

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestNormalizeModel(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5-5":                   "claude-opus-5-5",
		"Claude-Opus-5-5[1m]":               "claude-opus-5-5",
		"us.anthropic.claude-sonnet-5-5":    "claude-sonnet-5-5",
		"claude-haiku-4-5-20251001":         "claude-haiku-4-5",
		"claude-opus-4-5@20251101":          "claude-opus-4-5",
		"claude-opus-4.8":                   "claude-opus-4-8",
		"anthropic/claude-fable-5-1":        "claude-fable-5-1",
		"claude-sonnet-4-5-20250929-latest": "claude-sonnet-4-5",
		"  ":                                "",
	}
	for in, want := range cases {
		if got := normalizeModel(in); got != want {
			t.Errorf("normalizeModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPricingKey(t *testing.T) {
	if got := pricingKey("claude-opus-5-5", "fast", 10); got != "claude-opus-5-5+fast" {
		t.Errorf("fast key = %q", got)
	}
	if got := pricingKey("claude-haiku-5-5", "", 100_001); got != "claude-haiku-5-5+long" {
		t.Errorf("long key = %q", got)
	}
	if got := pricingKey("claude-haiku-5-5", "", 100_000); got != "claude-haiku-5-5" {
		t.Errorf("threshold key = %q", got)
	}
	if got := pricingKey("claude-opus-5-5", "", 500_000); got != "claude-opus-5-5" {
		t.Errorf("opus has no long tier, key = %q", got)
	}
	if got := pricingKey("", "", 0); got != "unknown" {
		t.Errorf("empty key = %q", got)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPriceOf(t *testing.T) {
	// 1M of each type on Opus 5.5: input 4, read 0.20, write 5m 1.25*4, write 1h 2*4, output 20.
	s := &modelStat{Input: 1e6, CacheRead: 1e6, CacheWrite5m: 1e6, CacheWrite1h: 1e6, Output: 1e6}
	c, ok := priceOf("claude-opus-5-5", s)
	if !ok {
		t.Fatal("opus 5.5 unpriced")
	}
	if !near(c.Input, 4) || !near(c.CacheRead, 0.2) || !near(c.CacheWrite, 5+8) || !near(c.Output, 20) {
		t.Errorf("breakdown = %+v", c)
	}
	if !near(c.Total, 4+0.2+13+20) {
		t.Errorf("total = %v", c.Total)
	}
	if !near(c.NoCache, 4+20+3*4) {
		t.Errorf("no cache = %v", c.NoCache)
	}
	fast, _ := priceOf("claude-opus-5-5+fast", s)
	if !near(fast.Total, 2*c.Total) {
		t.Errorf("fast total = %v, want %v", fast.Total, 2*c.Total)
	}
	long, _ := priceOf("claude-haiku-5-5+long", &modelStat{Input: 1e6, Output: 1e6, CacheRead: 1e6})
	if !near(long.Total, 0.5+2.5+0.05) {
		t.Errorf("haiku long total = %v", long.Total)
	}
	if _, ok := priceOf("claude-opus-4-8+fast", s); ok {
		t.Error("opus 4.8 fast should be unpriced")
	}
	if _, ok := priceOf("unknown", s); ok {
		t.Error("unknown model should be unpriced")
	}
}

func TestViewCost(t *testing.T) {
	h := &History{data: newHistoryData()}
	now := time.Date(2026, 11, 5, 15, 30, 0, 0, time.UTC)
	oneHour := int64(200_000)
	h.HandleUsage(context.Background(), usage.Record{
		Provider: "claude", AuthID: "a", Model: "claude-opus-5-5", RequestedAt: now.Add(-time.Hour),
		Detail: usage.Detail{
			InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 2_000_000,
			CacheCreationTokens: 500_000, CacheCreation1hTokens: &oneHour,
		},
	})
	// Legacy hour without per-model counts is priced as the assumed model, writes at 1h.
	h.data.Hours["a"][now.Add(-2*time.Hour).Truncate(time.Hour).Unix()] = &hourStat{Requests: 1, Input: 1000, CacheWrite: 1000}
	// An unknown model stays unpriced.
	h.HandleUsage(context.Background(), usage.Record{Provider: "claude", AuthID: "a", Model: "mystery", RequestedAt: now.Add(-time.Hour), Detail: usage.Detail{InputTokens: 500}})

	v := h.View("24h", now)
	acc := v.Accounts["a"]
	// input 4 + read 0.4 + write (300k*1.25 + 200k*2)*4/1e6 = 3.1 + output 2.
	tracked := 4 + 0.4 + 3.1 + 2.0
	legacy := (1000*4 + 1000*2*4) / 1e6
	want := tracked + legacy
	if !near(acc.Cost.Total, want) {
		t.Errorf("account cost = %v, want %v", acc.Cost.Total, want)
	}
	if acc.UnpricedTokens != 500 {
		t.Errorf("unpriced = %d, want 500", acc.UnpricedTokens)
	}
	if l := v.Models[legacyModelKey]; l.Requests != 1 || l.CacheWrite != 1000 || !near(l.Cost.Total, legacy) {
		t.Errorf("legacy totals = %+v", l)
	}
	var bucketSum float64
	for _, b := range v.Buckets {
		bucketSum += b.CostByAccount["a"]
		if !near(b.Cost["claude"], b.CostByAccount["a"]) {
			t.Errorf("bucket provider/account cost mismatch: %+v", b)
		}
	}
	if !near(bucketSum, want) {
		t.Errorf("bucket cost sum = %v", bucketSum)
	}
	m := v.Models["claude-opus-5-5"]
	if !m.Priced || m.Requests != 1 || m.CacheWrite != 500_000 || !near(m.Cost.Total, tracked) {
		t.Errorf("model totals = %+v", m)
	}
}

func TestMigrateV2ToV3KeepsCounts(t *testing.T) {
	data := newHistoryData()
	data.Version = 2
	data.Providers["a"] = "claude"
	data.Hours["a"] = map[int64]*hourStat{0: {Requests: 1, Cached: 100}}
	if !migrateHistory(&data) {
		t.Fatal("expected migration")
	}
	if data.Version != historyVersion || data.Hours["a"][0].Cached != 100 {
		t.Errorf("v2 data changed: %+v", data.Hours["a"][0])
	}
}

func TestViewByModelAndPrevious(t *testing.T) {
	h := &History{data: newHistoryData()}
	now := time.Date(2026, 11, 5, 15, 30, 0, 0, time.UTC)
	rec := func(at time.Time, model string, in int64) {
		h.HandleUsage(context.Background(), usage.Record{
			Provider: "claude", AuthID: "a", Model: model, RequestedAt: at,
			Detail: usage.Detail{InputTokens: in, OutputTokens: 1000},
		})
	}
	rec(now.Add(-time.Hour), "claude-opus-5-5", 1_000_000)
	rec(now.Add(-time.Hour), "claude-sonnet-5-5", 1_000_000)
	rec(now.Add(-30*time.Hour), "claude-opus-5-5", 500_000) // previous 24h window

	v := h.View("24h", now)
	var opus, sonnet float64
	var sonnetTok, sonnetReq int64
	for _, b := range v.Buckets {
		opus += b.CostByModel["claude-opus-5-5"]
		sonnet += b.CostByModel["claude-sonnet-5-5"]
		sonnetTok += b.TokensByModel["claude-sonnet-5-5"]
		sonnetReq += b.RequestsByModel["claude-sonnet-5-5"]
	}
	if !near(opus, 4+0.02) || !near(sonnet, 2+0.01) || sonnetTok != 1_001_000 || sonnetReq != 1 {
		t.Errorf("by model: opus %v sonnet %v tok %d req %d", opus, sonnet, sonnetTok, sonnetReq)
	}
	if !near(opus+sonnet, v.Accounts["a"].Cost.Total) {
		t.Errorf("model sum %v != account %v", opus+sonnet, v.Accounts["a"].Cost.Total)
	}
	p := v.Previous["a"]
	if p.Requests != 1 || p.Input != 500_000 || !near(p.Cost.Total, 2+0.02) {
		t.Errorf("previous = %+v", p)
	}
	if v.Accounts["a"].Requests != 2 {
		t.Errorf("previous leaked into range: %+v", v.Accounts["a"])
	}
	if v.PreviousComplete {
		t.Error("history starts inside the previous window, should be incomplete")
	}
	rec(now.Add(-60*time.Hour), "claude-opus-5-5", 1)
	if !h.View("24h", now).PreviousComplete {
		t.Error("history reaches before the previous window, should be complete")
	}
}

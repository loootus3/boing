package subsusage

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestCleanAccountName(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"  Pessoal  ", "Pessoal", true},
		{"Time\tdo\nRafa", "Time do Rafa", true},
		{"a\x00b", "a b", true},
		{"", "", true},
		{strings.Repeat("é", 32), strings.Repeat("é", 32), true},
		{strings.Repeat("x", 33), strings.Repeat("x", 33), false},
	}
	for _, c := range cases {
		got, ok := cleanAccountName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("cleanAccountName(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAccountName(t *testing.T) {
	if got := accountName(&coreauth.Auth{}); got != "" {
		t.Errorf("no metadata: %q", got)
	}
	if got := accountName(&coreauth.Auth{Metadata: map[string]any{MetadataUsageName: nil}}); got != "" {
		t.Errorf("cleared name: %q", got)
	}
	if got := accountName(&coreauth.Auth{Metadata: map[string]any{MetadataUsageName: " Trabalho "}}); got != "Trabalho" {
		t.Errorf("name: %q", got)
	}
	if got := accountName(&coreauth.Auth{Metadata: map[string]any{MetadataUsageName: strings.Repeat("y", 40)}}); got != strings.Repeat("y", 32) {
		t.Errorf("long name not capped: %q", got)
	}
}

func TestRoundCosts(t *testing.T) {
	v := HistoryView{
		Buckets:  []Bucket{{Cost: map[string]float64{"claude": 103.17907303999999}, CostByAccount: map[string]float64{"a": 0.1 + 0.2}, CostByModel: map[string]float64{}}},
		Accounts: map[string]AccountTotals{"a": {Cost: CostBreakdown{Total: 0.1 + 0.2, NoCache: 1.0000004}}},
		Previous: map[string]AccountTotals{},
		Models:   map[string]ModelTotals{"m": {Cost: CostBreakdown{Output: 2.0000006}}},
	}
	v.roundCosts()
	if got := v.Buckets[0].Cost["claude"]; got != 103.179073 {
		t.Errorf("bucket cost = %v", got)
	}
	if got := v.Buckets[0].CostByAccount["a"]; got != 0.3 {
		t.Errorf("bucket account cost = %v", got)
	}
	if c := v.Accounts["a"].Cost; c.Total != 0.3 || c.NoCache != 1 {
		t.Errorf("account cost = %+v", c)
	}
	if got := v.Models["m"].Cost.Output; got != 2.000001 {
		t.Errorf("model cost = %v", got)
	}
}

// An unchanged history gives byte-identical views, whatever the map iteration order.
func TestViewDeterministic(t *testing.T) {
	h := &History{data: newHistoryData()}
	now := time.Date(2026, 11, 5, 15, 30, 0, 0, time.UTC)
	models := []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-haiku-5-5+long"}
	for i := 0; i < 200; i++ {
		h.HandleUsage(context.Background(), usage.Record{
			Provider: "claude", AuthID: []string{"a", "b", "c"}[i%3], Model: models[i%len(models)],
			RequestedAt: now.Add(-time.Duration(i%40) * time.Hour),
			Detail:      usage.Detail{InputTokens: int64(1 + i*7919%10007), OutputTokens: int64(78623 + i), CachedTokens: int64(i * 104729 % 99991)},
		})
	}
	for _, rng := range []string{"24h", "7d", "30d"} {
		first, _ := json.Marshal(h.View(rng, now))
		for i := 0; i < 30; i++ {
			if got, _ := json.Marshal(h.View(rng, now)); !bytes.Equal(got, first) {
				t.Fatalf("%s view changed between identical calls", rng)
			}
		}
	}
}

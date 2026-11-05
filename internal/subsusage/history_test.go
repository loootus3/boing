package subsusage

import (
	"context"
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
	if last.Requests["claude"] != 2 || last.Requests["codex"] != 1 || last.Tokens["claude"] != 30 {
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

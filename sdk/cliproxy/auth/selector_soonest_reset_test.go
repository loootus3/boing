package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSortBySoonestReset(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	infos := map[string]ResetInfo{
		"late":      {ResetAt: now.Add(72 * time.Hour)},
		"soon":      {ResetAt: now.Add(2 * time.Hour)},
		"exhausted": {ResetAt: now.Add(time.Hour), Exhausted: true},
		"stale":     {ResetAt: now.Add(-time.Hour)},
	}
	SetResetInfoProvider(func(id string) (ResetInfo, bool) {
		info, ok := infos[id]
		return info, ok
	})
	t.Cleanup(func() { SetResetInfoProvider(nil) })

	auths := []*Auth{{ID: "exhausted"}, {ID: "late"}, {ID: "unknown"}, {ID: "stale"}, {ID: "soon"}}
	got := SortBySoonestReset(auths, now)
	want := []string{"soon", "late", "unknown", "stale", "exhausted"}
	for i, a := range got {
		if a.ID != want[i] {
			t.Fatalf("position %d: got %s, want %s (full: %v)", i, a.ID, want[i], ids(got))
		}
	}
}

func TestSoonestResetSelectorPick(t *testing.T) {
	now := time.Now()
	SetResetInfoProvider(func(id string) (ResetInfo, bool) {
		switch id {
		case "a":
			return ResetInfo{ResetAt: now.Add(48 * time.Hour)}, true
		case "b":
			return ResetInfo{ResetAt: now.Add(3 * time.Hour)}, true
		}
		return ResetInfo{}, false
	})
	t.Cleanup(func() { SetResetInfoProvider(nil) })

	auths := []*Auth{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}}
	got, err := (&SoonestResetSelector{}).Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick = %s, want b", got.ID)
	}
}

func ids(auths []*Auth) []string {
	out := make([]string, 0, len(auths))
	for _, a := range auths {
		out = append(out, a.ID)
	}
	return out
}

func TestSoonestResetSelectorSkipsOff(t *testing.T) {
	SetResetInfoProvider(nil)
	off := &Auth{ID: "a", Provider: "claude", Metadata: map[string]any{MetadataUsageMode: UsageModeOff}}
	on := &Auth{ID: "b", Provider: "claude"}
	got, err := (&SoonestResetSelector{}).Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{off, on})
	if err != nil || got.ID != "b" {
		t.Fatalf("Pick = %v, %v; want b", got, err)
	}
	if _, err := (&SoonestResetSelector{}).Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{off}); err == nil {
		t.Fatal("expected an error when only off credentials are available")
	}
}

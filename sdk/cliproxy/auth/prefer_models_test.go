package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestAuthPrefersModel(t *testing.T) {
	a := &Auth{Metadata: map[string]any{MetadataPreferModels: []any{"claude-fable-5-2*"}}}
	for model, want := range map[string]bool{
		"claude-fable-5-2":      true,
		"Claude-Fable-5-2[1m]":  true,
		"claude-fable-5-1":      false,
		"claude-opus-5-5":       false,
		"":                      false,
		"x-claude-fable-5-2":    false,
		"claude-fable-5-2-fast": true,
	} {
		if got := AuthPrefersModel(a, model); got != want {
			t.Errorf("%q: got %v want %v", model, got, want)
		}
	}
	if !globMatch("a*b*c", "axxbyyc") || globMatch("a*b*c", "axxc") || !globMatch("abc", "abc") {
		t.Fatal("globMatch")
	}
}

func fableAuth(id string, rank int, extra map[string]any) *Auth {
	md := map[string]any{MetadataPreferModels: []any{"claude-fable-5-2*"}, MetadataPreferRank: float64(rank)}
	for k, v := range extra {
		md[k] = v
	}
	return &Auth{ID: id, Provider: "claude", Metadata: md, Attributes: map[string]string{}}
}

func TestSoonestResetModelOrder(t *testing.T) {
	plain := &Auth{ID: "plain", Provider: "claude", Attributes: map[string]string{}}
	// "Use last" account (lower priority tier) placed first for Fable.
	last := fableAuth("last", 1, map[string]any{MetadataUsageMode: UsageModeLast})
	last.Attributes["priority"] = "-100"
	second := fableAuth("second", 2, nil)
	off := fableAuth("off", 1, map[string]any{MetadataUsageMode: UsageModeOff})
	s := &SoonestResetSelector{}
	ctx := context.WithValue(context.Background(), prevalidatedAuthCandidatesKey{}, true)
	pick := func(model string, auths ...*Auth) string {
		got, err := s.Pick(ctx, "claude", model, cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatal(err)
		}
		return got.ID
	}
	if id := pick("claude-fable-5-2", plain, second, last); id != "last" {
		t.Fatalf("fable picked %s, want rank 1 from the lower tier", id)
	}
	if id := pick("claude-fable-5-2", plain, second); id != "second" {
		t.Fatalf("fable picked %s, want rank 2 when rank 1 is unavailable", id)
	}
	if id := pick("claude-opus-5-5", last, plain); id != "plain" {
		t.Fatalf("other model picked %s, want the highest tier", id)
	}
	if id := pick("claude-fable-5-2", plain, off); id != "plain" {
		t.Fatalf("an off credential must never be used, picked %s", id)
	}
	if !anyAuthPrefersModel([]*Auth{plain, second}, "claude-fable-5-2") || anyAuthPrefersModel([]*Auth{plain}, "claude-fable-5-2") {
		t.Fatal("anyAuthPrefersModel")
	}
}

func TestPreferModelAuthsSkipsExhausted(t *testing.T) {
	prev := resetInfoProvider.Load()
	fn := func(id string) (ResetInfo, bool) { return ResetInfo{Exhausted: id == "full"}, id == "full" }
	SetResetInfoProvider(fn)
	t.Cleanup(func() {
		if prev != nil {
			resetInfoProvider.Store(prev)
		} else {
			SetResetInfoProvider(nil)
		}
	})
	full, ok := fableAuth("full", 1, nil), fableAuth("ok", 2, nil)
	if got := preferModelAuths([]*Auth{full, ok}, "claude-fable-5-2"); len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("got %v", got)
	}
}

func TestSoonestResetSkipsFableExhaustedForFable(t *testing.T) {
	prev := resetInfoProvider.Load()
	fn := func(id string) (ResetInfo, bool) { return ResetInfo{FableExhausted: id == "spent"}, id == "spent" }
	SetResetInfoProvider(fn)
	t.Cleanup(func() {
		if prev != nil {
			resetInfoProvider.Store(prev)
		} else {
			SetResetInfoProvider(nil)
		}
	})
	s := &SoonestResetSelector{}
	ctx := context.WithValue(context.Background(), prevalidatedAuthCandidatesKey{}, true)
	spent, next := fableAuth("spent", 1, nil), fableAuth("next", 2, nil)
	pick := func(model string, auths ...*Auth) string {
		got, err := s.Pick(ctx, "claude", model, cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatal(err)
		}
		return got.ID
	}
	if got := pick("claude-fable-5-2", spent, next); got != "next" {
		t.Fatalf("fable picked %s", got)
	}
	// Other models still use the account; with no alternative, Fable keeps it rather than failing here.
	if got := pick("claude-fable-5-2", spent); got != "spent" {
		t.Fatalf("only account: picked %s", got)
	}
	plain := &Auth{ID: "spent", Provider: "claude", Attributes: map[string]string{}}
	if got := pick("claude-opus-5-5", plain); got != "spent" {
		t.Fatalf("opus picked %s", got)
	}
	// Every ranked account spent: Fable falls to an account outside the order, any letter case.
	other := &Auth{ID: "other", Provider: "claude", Attributes: map[string]string{}}
	if got := pick("Claude-Fable-5-2", spent, other); got != "other" {
		t.Fatalf("fallback picked %s", got)
	}
}

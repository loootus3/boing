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

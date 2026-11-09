package subsusage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// fablePatterns are the prefer_models patterns written for accounts in the Fable 5.2 order.
var fablePatterns = []any{"claude-fable-5-2*"}

// fableRankOf returns the credential's position in the Fable 5.2 order, 0 when not in it.
func fableRankOf(a *coreauth.Auth) int {
	if !coreauth.AuthPrefersModel(a, "claude-fable-5-2") {
		return 0
	}
	return coreauth.AuthPreferRank(a)
}

// SetFableOrder makes the listed Claude accounts serve Fable 5.2 in that order (from any
// priority, so a "use last" account can come first), and removes every other account from it.
// The selector falls back to normal routing when none of the listed accounts is available.
func (t *Tracker) SetFableOrder(ctx context.Context, order []string) error {
	t.mu.RLock()
	m := t.manager
	t.mu.RUnlock()
	if m == nil {
		return errors.New("auth manager not ready")
	}
	rank := make(map[string]int, len(order))
	for i, id := range order {
		id = strings.TrimSpace(id)
		if _, dup := rank[id]; dup || id == "" {
			return fmt.Errorf("invalid or repeated account %q", id)
		}
		a, ok := m.GetByID(id)
		if !ok || a == nil || !strings.EqualFold(a.Provider, "claude") {
			return fmt.Errorf("unknown Claude account %q", id)
		}
		rank[id] = i + 1
	}
	for _, a := range m.List() {
		if a == nil || !strings.EqualFold(a.Provider, "claude") {
			continue
		}
		want := rank[a.ID]
		// Accounts outside the order are only touched when they are in it now, so an order kept
		// by hand for other models is left alone.
		if want == 0 && !coreauth.AuthPrefersModel(a, "claude-fable-5-2") || want > 0 && fableRankOf(a) == want {
			continue
		}
		_, _, err := m.Modify(ctx, a.ID, 0, func(x *coreauth.Auth) error {
			md := make(map[string]any, len(x.Metadata)+2)
			for k, v := range x.Metadata {
				md[k] = v
			}
			if want > 0 {
				md[coreauth.MetadataPreferModels] = fablePatterns
				md[coreauth.MetadataPreferRank] = want
			} else {
				delete(md, coreauth.MetadataPreferModels)
				delete(md, coreauth.MetadataPreferRank)
			}
			x.Metadata = md
			return nil
		})
		if err != nil {
			return fmt.Errorf("save %s: %w", a.ID, err)
		}
	}
	return nil
}

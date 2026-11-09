package auth

import (
	"sort"
	"strings"
	"time"
)

// MetadataPreferModels is the auth-file field listing model patterns ('*' matches any run of
// characters, case-insensitive) that have their own credential order. A request for a matching
// model goes to the first available credential of that order, by MetadataPreferRank and from
// any priority tier, and falls back to normal routing only when none of them is available.
// Other models are routed as usual.
const MetadataPreferModels = "prefer_models"

// MetadataPreferRank is the credential's position in that order: 1 first, then 2, and so on.
const MetadataPreferRank = "prefer_rank"

// AuthPrefersModel reports whether the credential lists a pattern matching model.
func AuthPrefersModel(a *Auth, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if a == nil || a.Metadata == nil || model == "" {
		return false
	}
	var patterns []string
	switch v := a.Metadata[MetadataPreferModels].(type) {
	case []string:
		patterns = v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				patterns = append(patterns, s)
			}
		}
	case string:
		patterns = strings.Split(v, ",")
	}
	for _, p := range patterns {
		if globMatch(strings.ToLower(strings.TrimSpace(p)), model) {
			return true
		}
	}
	return false
}

// AuthPreferRank returns the credential's MetadataPreferRank, or 0 when unset or invalid.
func AuthPreferRank(a *Auth) int {
	if a == nil || a.Metadata == nil {
		return 0
	}
	switch v := a.Metadata[MetadataPreferRank].(type) {
	case int:
		return max(v, 0)
	case int64:
		return int(max(v, 0))
	case float64:
		return int(max(v, 0))
	}
	return 0
}

// globMatch matches value against pattern, where '*' matches any (possibly empty) substring.
func globMatch(pattern, value string) bool {
	if pattern == "" {
		return false
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(value, mid)
		if i < 0 {
			return false
		}
		value = value[i+len(mid):]
	}
	return strings.HasSuffix(value, last)
}

func anyAuthPrefersModel(auths []*Auth, model string) bool {
	if strings.TrimSpace(model) == "" {
		return false
	}
	for _, a := range auths {
		if AuthPrefersModel(a, model) {
			return true
		}
	}
	return false
}

// preferModelAuths returns the credentials that have model in their own order.
func preferModelAuths(auths []*Auth, model string) []*Auth {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	var preferred []*Auth
	for _, a := range auths {
		if AuthPrefersModel(a, model) {
			preferred = append(preferred, a)
		}
	}
	return preferred
}

// sortByPreferRank orders credentials by MetadataPreferRank (unset last), ties by soonest reset.
func sortByPreferRank(auths []*Auth, now time.Time) []*Auth {
	out := SortBySoonestReset(auths, now)
	rank := func(a *Auth) int {
		if r := AuthPreferRank(a); r > 0 {
			return r
		}
		return int(^uint(0) >> 1)
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

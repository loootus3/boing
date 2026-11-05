package subsusage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Endpoints and headers mirror the official management panel
// (router-for-me/Cli-Proxy-API-Management-Center, src/utils/quota/constants.ts).
const (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
	claudeUserAgent  = "claude-cli/2.1.280 (external, cli)"
	codexUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
	codexUserAgent   = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
)

// claudeWindows lists the Claude usage windows in display order.
var claudeWindows = []struct{ key, kind, label string }{
	{"five_hour", "short", "5h"},
	{"seven_day", "weekly", "Semanal"},
	{"seven_day_opus", "model", "Opus"},
	{"seven_day_sonnet", "model", "Sonnet"},
	{"seven_day_oauth_apps", "model", "Apps"},
	{"seven_day_cowork", "model", "Cowork"},
}

const (
	fiveHourSeconds = 5 * 3600
	weekSeconds     = 7 * 24 * 3600
)

func (t *Tracker) fetchClaude(ctx context.Context, authID, token string, entry *Entry) error {
	headers := map[string]string{
		"Authorization":  "Bearer " + token,
		"Content-Type":   "application/json",
		"Accept":         "application/json",
		"anthropic-beta": "oauth-2025-04-20",
		"User-Agent":     claudeUserAgent,
	}
	body, err := t.get(ctx, claudeUsageURL, headers)
	if err != nil {
		return err
	}
	if errParse := parseClaudeUsage(body, entry); errParse != nil {
		return errParse
	}
	// The plan comes from the profile endpoint, which is only called when the cached plan is
	// missing or older than planTTL. Failures there are not fatal and keep the previous plan.
	cached, ok := t.cachedPlanOf(authID)
	entry.Plan = cached.plan
	if ok && cached.plan != "" && time.Since(cached.at) < planTTL {
		return nil
	}
	delete(headers, "Accept")
	if profile, errProfile := t.get(ctx, claudeProfileURL, headers); errProfile == nil {
		entry.Plan = claudePlan(profile)
		t.storePlan(authID, entry.Plan)
	}
	return nil
}

func parseClaudeUsage(body []byte, entry *Entry) error {
	for _, spec := range claudeWindows {
		node := ojson.GetBytes(body, spec.key)
		if !node.IsObject() || !node.Get("utilization").Exists() {
			continue
		}
		w := Window{Kind: spec.kind, Label: spec.label, UsedPercent: node.Get("utilization").Float(), DurationSec: weekSeconds}
		if spec.kind == "short" {
			w.DurationSec = fiveHourSeconds
		}
		w.ResetAt = parseResetTime(node.Get("resets_at"))
		entry.Windows = append(entry.Windows, w)
	}
	if len(entry.Windows) == 0 {
		return fmt.Errorf("resposta sem janelas de uso")
	}
	return nil
}

func claudePlan(profile []byte) string {
	org := ojson.GetBytes(profile, "organization")
	if org.Get("organization_type").String() == "claude_team" && org.Get("subscription_status").String() == "active" {
		return "Team"
	}
	tier := org.Get("rate_limit_tier").String()
	switch {
	case strings.Contains(tier, "ultra"), strings.HasSuffix(tier, "claude_max_20x"):
		// Anthropic renamed the 20x Max tier to Ultra.
		return "Ultra"
	case strings.HasSuffix(tier, "claude_max_5x"):
		return "Max 5x"
	case ojson.GetBytes(profile, "account.has_claude_max").Bool():
		return "Max"
	case ojson.GetBytes(profile, "account.has_claude_pro").Bool():
		return "Pro"
	}
	return "Free"
}

func (t *Tracker) fetchCodex(ctx context.Context, token, accountID string, entry *Entry) error {
	headers := map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
		"User-Agent":    codexUserAgent,
	}
	if accountID != "" {
		headers["Chatgpt-Account-Id"] = accountID
	}
	body, err := t.get(ctx, codexUsageURL, headers)
	if err != nil {
		return err
	}
	return parseCodexUsage(body, entry, time.Now())
}

func parseCodexUsage(body []byte, entry *Entry, now time.Time) error {
	if plan := strings.ToLower(firstString(body, "plan_type", "planType")); plan != "" {
		entry.Plan = strings.ToUpper(plan[:1]) + plan[1:]
	}
	rate := ojson.GetBytes(body, "rate_limit")
	if !rate.Exists() {
		rate = ojson.GetBytes(body, "rateLimit")
	}
	limitReached := rate.Get("limit_reached").Bool() || (rate.Get("allowed").Exists() && !rate.Get("allowed").Bool())
	entry.LimitReached = limitReached

	for i, key := range []string{"primary_window", "secondary_window"} {
		node := rate.Get(key)
		if !node.IsObject() {
			continue
		}
		w := Window{UsedPercent: node.Get("used_percent").Float()}
		secs := node.Get("limit_window_seconds").Int()
		w.DurationSec = secs
		switch {
		case secs > 0 && secs <= 24*3600:
			w.Kind, w.Label = "short", fmt.Sprintf("%dh", (secs+1800)/3600)
		case secs >= 28*24*3600:
			w.Kind, w.Label = "monthly", "Mensal"
		case secs > 0:
			w.Kind, w.Label = "weekly", "Semanal"
		case !node.Get("used_percent").Exists() && !node.Get("reset_at").Exists() && !node.Get("reset_after_seconds").Exists():
			// An empty window object without a duration is not a real limit.
			continue
		case i == 0:
			w.Kind, w.Label, w.DurationSec = "short", "5h", fiveHourSeconds
		default:
			w.Kind, w.Label, w.DurationSec = "weekly", "Semanal", weekSeconds
		}
		w.ResetAt = parseResetTime(node.Get("reset_at"))
		if w.ResetAt == nil {
			if after := node.Get("reset_after_seconds").Int(); after > 0 {
				ts := now.Add(time.Duration(after) * time.Second).UTC()
				w.ResetAt = &ts
			}
		}
		if !node.Get("used_percent").Exists() && limitReached {
			w.UsedPercent = 100
		}
		entry.Windows = append(entry.Windows, w)
	}
	if len(entry.Windows) == 0 {
		return fmt.Errorf("resposta sem janelas de uso")
	}
	return nil
}

func firstString(body []byte, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(ojson.GetBytes(body, k).String()); v != "" {
			return v
		}
	}
	return ""
}

// parseResetTime accepts ISO-8601 strings and Unix seconds or milliseconds.
func parseResetTime(v ojson.Result) *time.Time {
	if !v.Exists() {
		return nil
	}
	raw := strings.TrimSpace(v.String())
	if raw == "" || raw == "null" {
		return nil
	}
	if n, err := strconv.ParseFloat(raw, 64); err == nil && n > 0 {
		var ts time.Time
		if n < 1e11 {
			ts = time.Unix(int64(n), 0)
		} else {
			ts = time.UnixMilli(int64(n))
		}
		ts = ts.UTC()
		return &ts
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07:00"} {
		if ts, err := time.Parse(layout, raw); err == nil {
			ts = ts.UTC()
			return &ts
		}
	}
	return nil
}

// codexAccountID finds the ChatGPT account id like the official panel does: explicit
// fields first, then the id_token JWT claims.
func codexAccountID(a *coreauth.Auth) string {
	for _, key := range []string{"account_id", "chatgpt_account_id", "chatgptAccountId"} {
		if v := metaString(a, key); v != "" {
			return v
		}
		if a != nil && a.Attributes != nil {
			if v := strings.TrimSpace(a.Attributes[key]); v != "" {
				return v
			}
		}
	}
	if idToken := metaString(a, "id_token"); idToken != "" {
		if claims, err := codex.ParseJWTToken(idToken); err == nil && claims != nil {
			return claims.GetAccountID()
		}
	}
	return ""
}

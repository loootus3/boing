package subsusage

import (
	"regexp"
	"strings"
)

// reCLIEntry matches Claude Code's user agent, e.g. "claude-cli/2.1.3 (external, sdk-ts)", and
// captures the entrypoint (cli, sdk-ts, sdk-py, claude-vscode, ...).
var reCLIEntry = regexp.MustCompile(`^claude-cli/\S+\s*\(([^)]*)\)`)

// clientLabel names the client app behind a request from its User-Agent. Unknown agents keep
// their product token so new clients show up on the page instead of disappearing into "other".
func clientLabel(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "desconhecido"
	}
	if m := reCLIEntry.FindStringSubmatch(ua); m != nil {
		parts := strings.Split(m[1], ",")
		entry := strings.TrimSpace(parts[len(parts)-1])
		switch {
		case entry == "cli":
			return "Claude Code"
		case strings.HasPrefix(entry, "sdk-"):
			return "Agent SDK (" + strings.TrimPrefix(entry, "sdk-") + ")"
		case strings.Contains(entry, "vscode"):
			return "Claude Code (VS Code)"
		case strings.Contains(entry, "jetbrains"):
			return "Claude Code (JetBrains)"
		case entry != "":
			return "Claude Code (" + capLabel(entry) + ")"
		}
		return "Claude Code"
	}
	lower := strings.ToLower(ua)
	switch {
	case strings.HasPrefix(lower, "anthropic/"):
		return "SDK Anthropic"
	case strings.HasPrefix(lower, "claude-cli/"):
		return "Claude Code"
	}
	product, _, _ := strings.Cut(ua, " ")
	product, _, _ = strings.Cut(product, "/")
	return capLabel(product)
}

func capLabel(s string) string {
	if r := []rune(s); len(r) > 32 {
		return string(r[:32])
	}
	return s
}

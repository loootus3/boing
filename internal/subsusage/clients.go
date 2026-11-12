package subsusage

import (
	"regexp"
	"strings"
)

// reCLIEntry matches Claude Code's user agent, e.g. "claude-cli/2.1.3 (external, sdk-ts)" or
// "claude-cli/2.1.3 (external, agent-sdk/0.3.321, client-app/t3)", and captures the part in parentheses.
var reCLIEntry = regexp.MustCompile(`^claude-cli/\S+\s*\(([^)]*)\)`)

// clientLabel names the client app behind a request from its User-Agent. Unknown agents keep
// their product token so new clients show up on the page instead of disappearing into "other".
func clientLabel(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "desconhecido"
	}
	if m := reCLIEntry.FindStringSubmatch(ua); m != nil {
		entry, app := "", ""
		for _, part := range strings.Split(m[1], ",") {
			name, val, _ := strings.Cut(strings.TrimSpace(part), "/")
			switch {
			case name == "" || name == "external":
			case name == "client-app" || name == "app":
				app = val
			case entry == "":
				entry = name
			}
		}
		label := "Claude Code"
		switch {
		case entry == "" || entry == "cli":
		case strings.HasPrefix(entry, "sdk") || strings.HasPrefix(entry, "agent-sdk"):
			label = "Agent SDK"
		case strings.Contains(entry, "vscode"):
			label = "Claude Code (VS Code)"
		case strings.Contains(entry, "jetbrains"):
			label = "Claude Code (JetBrains)"
		default:
			label = "Claude Code (" + capLabel(entry) + ")"
		}
		if app != "" {
			label += " · " + capLabel(app)
		}
		return label
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

// relabelClient maps labels stored by earlier versions of clientLabel to the current ones.
func relabelClient(label string) string {
	if strings.HasPrefix(label, "Claude Code (agent-sdk") || strings.HasPrefix(label, "Agent SDK (") {
		return "Agent SDK"
	}
	return label
}

func capLabel(s string) string {
	if r := []rune(s); len(r) > 32 {
		return string(r[:32])
	}
	return s
}

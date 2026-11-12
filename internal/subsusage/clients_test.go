package subsusage

import (
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestClientLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.1.3 (external, cli)":                              "Claude Code",
		"claude-cli/2.1.3 (external, sdk-ts)":                           "Agent SDK",
		"claude-cli/2.1.3 (external, agent-sdk/0.3.321)":                "Agent SDK",
		"claude-cli/2.1.3 (external, agent-sdk/0.3.321, client-app/t3)": "Agent SDK · t3",
		"claude-cli/2.1.3 (external, claude-vscode)":                    "Claude Code (VS Code)",
		"Anthropic/JS 0.60.0":                                           "SDK Anthropic",
		"python-httpx/0.27":                                             "python-httpx",
		"":                                                              "desconhecido",
	} {
		if got := clientLabel(ua); got != want {
			t.Errorf("clientLabel(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestFailureReason(t *testing.T) {
	for _, c := range []struct {
		status int
		err    string
		want   string
	}{
		{0, `Post "x": claude tls: dial upstream: dial tcp: lookup api.anthropic.com on 127.0.0.53:53: server misbehaving`, "dns"},
		{0, "read tcp [::1]:1->[::2]:443: read: connection timed out", "timeout"},
		{0, "dial tcp [2607:6bc0::10]:443: connect: network is unreachable", "network"},
		{429, "rate limited", "429"},
		{529, "overloaded", "overloaded"},
		{401, "invalid", "auth"},
	} {
		if got := failureReason(c.status, c.err); got != c.want {
			t.Errorf("failureReason(%d, %q) = %q, want %q", c.status, c.err, got, c.want)
		}
	}
}

func TestHealthHookParsesConductorWarning(t *testing.T) {
	health.mu.Lock()
	health.buckets = make(map[int64]*healthStat)
	health.mu.Unlock()
	msg := `upstream execution failed: provider=claude model=claude-opus-5-5 auth=provider=claude auth_file=claude-1-a@b.json duration=29ms err=Post "https://api.anthropic.com/v1/messages": lookup api.anthropic.com: server misbehaving`
	_ = healthHook{}.Fire(logEntry(msg))
	_ = healthHook{}.Fire(logEntry(`429 |          1s | upstream execution failed: provider=claude model=m auth=provider=claude auth_file=claude-2-c@d.json err=limit`))
	v := health.view()
	if len(v) != 1 || v[0].Upstream["dns"] != 1 || v[0].Upstream["429"] != 1 || v[0].Accounts["claude-1-a@b.json"] != 1 || v[0].Accounts["claude-2-c@d.json"] != 1 {
		t.Fatalf("view = %+v", v)
	}
}

func logEntry(msg string) *log.Entry { return &log.Entry{Message: msg} }

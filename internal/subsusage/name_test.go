package subsusage

import (
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCleanAccountName(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"  Pessoal  ", "Pessoal", true},
		{"Time\tdo\nRafa", "Time do Rafa", true},
		{"a\x00b", "a b", true},
		{"", "", true},
		{strings.Repeat("é", 32), strings.Repeat("é", 32), true},
		{strings.Repeat("x", 33), strings.Repeat("x", 33), false},
	}
	for _, c := range cases {
		got, ok := cleanAccountName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("cleanAccountName(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAccountName(t *testing.T) {
	if got := accountName(&coreauth.Auth{}); got != "" {
		t.Errorf("no metadata: %q", got)
	}
	if got := accountName(&coreauth.Auth{Metadata: map[string]any{MetadataUsageName: nil}}); got != "" {
		t.Errorf("cleared name: %q", got)
	}
	if got := accountName(&coreauth.Auth{Metadata: map[string]any{MetadataUsageName: " Trabalho "}}); got != "Trabalho" {
		t.Errorf("name: %q", got)
	}
	if got := accountName(&coreauth.Auth{Metadata: map[string]any{MetadataUsageName: strings.Repeat("y", 40)}}); got != strings.Repeat("y", 32) {
		t.Errorf("long name not capped: %q", got)
	}
}

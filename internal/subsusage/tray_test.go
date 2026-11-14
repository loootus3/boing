package subsusage

import "testing"

func TestParseTrayBuild(t *testing.T) {
	cases := map[string]string{
		"MZ\x00junk TRAYBUILD:abc123:END tail": "abc123",
		"TRAYBUILD:bad id:END TRAYBUILD:ok-2:END": "ok-2",
		"no mark here":                            "",
		"TRAYBUILD::END":                          "",
	}
	for in, want := range cases {
		if got := parseTrayBuild([]byte(in)); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

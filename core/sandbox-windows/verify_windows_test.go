package windows

import "testing"

func TestProbeOutputMatches(t *testing.T) {
	cases := map[string]struct {
		output string
		want   string
		ok     bool
	}{
		"exact":                 {"blocked,blocked", "blocked,blocked", true},
		"crlf":                  {"blocked,blocked\r\n", "blocked,blocked", true},
		"trailing space":        {"blocked,blocked,ok \r\n", "blocked,blocked,ok", true},
		"trailing tab":          {"blocked,blocked,ok\t", "blocked,blocked,ok", true},
		"trailing vertical tab": {"blocked,blocked,ok\v", "blocked,blocked,ok", true},
		"noise before":          {"#< CLIXML\r\nblocked,blocked,ok\r\n", "blocked,blocked,ok", true},
		"noise after":           {"blocked,blocked,ok\r\n#< CLIXML\r\n", "blocked,blocked,ok", true},
		"partial result":        {"blocked,blocked,", "blocked,blocked,ok", false},
		"short result":          {"blocked,blocked", "blocked,blocked,ok", false},
		"extra token":           {"blocked,blocked,ok,ok", "blocked,blocked,ok", false},
		"empty":                 {"", "blocked,blocked", false},
		"only noise":            {"#< CLIXML\r\n", "blocked,blocked", false},
	}
	for name, tc := range cases {
		if got := probeOutputMatches(tc.output, tc.want); got != tc.ok {
			t.Errorf("%s: probeOutputMatches(%q, %q) = %v, want %v",
				name, tc.output, tc.want, got, tc.ok)
		}
	}
}

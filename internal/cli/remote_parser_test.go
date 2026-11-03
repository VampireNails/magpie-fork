package cli

import "testing"

func TestRemoteParserForms(t *testing.T) {
	cases := []struct {
		args                []string
		op, url             string
		json, noOpen, valid bool
	}{
		{[]string{"connect"}, "connect", "", false, false, true},
		{[]string{"connect", "https://node.example:9443"}, "connect", "https://node.example:9443", false, false, true},
		{[]string{"status", "--json"}, "status", "", true, false, true},
		{[]string{"manage", "--no-open"}, "manage", "", false, true, true},
		{[]string{"run"}, "run", "", false, false, true}, {[]string{"refresh"}, "refresh", "", false, false, true}, {[]string{"disconnect"}, "disconnect", "", false, false, true},
		{nil, "", "", false, false, false}, {[]string{"connect", "--json"}, "", "", false, false, false},
		{[]string{"status", "--no-open"}, "", "", false, false, false}, {[]string{"run", "extra"}, "", "", false, false, false},
		{[]string{"connect", "one", "two"}, "", "", false, false, false},
	}
	for _, c := range cases {
		got, err := parseRemote(c.args)
		if (err == nil) != c.valid {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if c.valid && (got.operation != c.op || got.serverURL != c.url || got.json != c.json || got.noOpen != c.noOpen) {
			t.Errorf("%q: %+v", c.args, got)
		}
	}
}

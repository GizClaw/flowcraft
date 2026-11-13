package permission

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Writes.Validate is the input to every sandbox projection, so it is the one
// place a bad path has to be refused.
func TestWritesValidate(t *testing.T) {
	valid := [][]string{
		nil,
		{"/"},
		{"/work"},
		{"/work", "/tmp/out"},
	}
	for _, paths := range valid {
		if err := (Writes{Paths: paths}).Validate(); err != nil {
			t.Errorf("Writes%v rejected: %v", paths, err)
		}
	}

	sep := string(filepath.Separator)
	invalid := map[string]Writes{
		"relative":        {Paths: []string{"work"}},
		"glob star":       {Paths: []string{"/work/*"}},
		"glob question":   {Paths: []string{"/work/?"}},
		"glob class":      {Paths: []string{"/work/[a-z]"}},
		"duplicate":       {Paths: []string{"/work", "/work"}},
		"duplicate clean": {Paths: []string{"/work", "/work/" + sep}},
	}
	for name, w := range invalid {
		err := w.Validate()
		if err == nil {
			t.Errorf("%s: accepted %v", name, w.Paths)
			continue
		}
		if !errdefs.IsValidation(err) {
			t.Errorf("%s: err = %v, want Validation", name, err)
		}
		if !strings.Contains(err.Error(), "permission:") {
			t.Errorf("%s: error %q does not name the package", name, err)
		}
	}
}

func TestWritesWritableIsACopy(t *testing.T) {
	w := Writes{Paths: []string{"/work"}}
	got := w.Writable()
	if len(got) != 1 || got[0] != "/work" {
		t.Fatalf("Writable = %v", got)
	}
	got[0] = "/elsewhere"
	if w.Paths[0] != "/work" {
		t.Fatal("Writable returned the backing slice; a caller could widen its own boundary")
	}
	if !(Writes{}).IsZero() {
		t.Error("the zero Writes is not reported as zero")
	}
}

func TestNetPolicyValidateAndIsZero(t *testing.T) {
	valid := []NetPolicy{
		{},
		{Mode: NetDefault},
		{Mode: NetDenyAll},
		{Mode: NetAllowList, AllowHosts: []string{"example.com", "1.2.3.4", "10.0.0.0/8"}},
		{Mode: NetProxy, Proxy: "http://127.0.0.1:3128"},
		{Mode: NetProxy, Proxy: "socks5://user:pass@127.0.0.1:1080"},
		{Mode: NetAllowList, Rules: []NetRule{
			{Action: NetAllow, Host: "example.com", Port: 443},
			{Action: NetDeny, Host: "*.ads.test", Reason: "tracker"},
		}},
		{Mode: NetAllowList, UnixSockets: []string{"/run/service.sock"}},
		{MITM: &MITMPolicy{Hosts: []string{"example.com"}, MaxBodyBytes: 1 << 20}},
	}
	for i, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("case %d rejected: %v", i, err)
		}
	}

	invalid := map[string]NetPolicy{
		"unknown mode":            {Mode: NetProxy + 1},
		"proxy wrong scheme":      {Mode: NetProxy, Proxy: "ftp://127.0.0.1:21"},
		"proxy without host":      {Mode: NetProxy, Proxy: "http://"},
		"proxy password no user":  {Mode: NetProxy, Proxy: "socks5://:pass@127.0.0.1:1080"},
		"rule bad action":         {Rules: []NetRule{{Action: NetAction(9), Host: "example.com"}}},
		"rule empty host":         {Rules: []NetRule{{Action: NetAllow, Host: "  "}}},
		"rule port out of range":  {Rules: []NetRule{{Action: NetAllow, Host: "example.com", Port: 70000}}},
		"rule negative port":      {Rules: []NetRule{{Action: NetAllow, Host: "example.com", Port: -1}}},
		"rule reason whitespace":  {Rules: []NetRule{{Action: NetDeny, Host: "example.com", Reason: " padded"}}},
		"rule reason multiline":   {Rules: []NetRule{{Action: NetDeny, Host: "example.com", Reason: "two\nlines"}}},
		"unix socket relative":    {UnixSockets: []string{"run/service.sock"}},
		"mitm negative body cap":  {MITM: &MITMPolicy{MaxBodyBytes: -1}},
		"mitm empty host":         {MITM: &MITMPolicy{Hosts: []string{" "}}},
		"mitm empty exclude host": {MITM: &MITMPolicy{ExcludeHosts: []string{""}}},
	}
	for name, policy := range invalid {
		err := policy.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errdefs.IsValidation(err) {
			t.Errorf("%s: err = %v, want Validation", name, err)
		}
	}

	zero := NetPolicy{}
	if !zero.IsZero() {
		t.Error("the zero NetPolicy is not reported as zero")
	}
	for name, policy := range map[string]NetPolicy{
		"mode":         {Mode: NetDenyAll},
		"allow hosts":  {AllowHosts: []string{"example.com"}},
		"rules":        {Rules: []NetRule{{Action: NetAllow, Host: "example.com"}}},
		"proxy":        {Proxy: "http://127.0.0.1:3128"},
		"unix sockets": {UnixSockets: []string{"/run/service.sock"}},
		"mitm":         {MITM: &MITMPolicy{}},
	} {
		if policy.IsZero() {
			t.Errorf("%s: reported as zero", name)
		}
	}
}

func TestPolicyValidateAggregates(t *testing.T) {
	policy := Policy{
		Writes: Writes{Paths: []string{"relative"}},
		Net:    NetPolicy{Mode: NetProxy + 1},
	}
	if err := policy.Validate(); err == nil {
		t.Fatal("Policy.Validate accepted an invalid writes list")
	}

	if !(Policy{}).IsZero() {
		t.Error("the zero Policy is not reported as zero")
	}
	policy = Policy{Writes: Writes{Paths: []string{"/work"}}}
	if policy.IsZero() {
		t.Error("a policy with a writable root is reported as zero")
	}
}

func TestIsolatingNetModes(t *testing.T) {
	modes := IsolatingNetModes()
	if len(modes) != 3 {
		t.Fatalf("IsolatingNetModes = %v, want three postures", modes)
	}
	for _, mode := range modes {
		if mode == NetDefault {
			t.Fatal("NetDefault is in the isolated set; it is the absence of a policy")
		}
	}
	// A caller mutating its copy must not change the next answer.
	modes[0] = NetDefault
	if got := IsolatingNetModes(); got[0] == NetDefault {
		t.Fatal("IsolatingNetModes returned shared state")
	}
}

func TestNetActionString(t *testing.T) {
	for action, want := range map[NetAction]string{NetDeny: "deny", NetAllow: "allow", NetAction(7): "unknown"} {
		if got := action.String(); got != want {
			t.Errorf("NetAction(%d).String() = %q, want %q", int(action), got, want)
		}
	}
}

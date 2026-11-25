package policy_test

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
)

// documentTypes are the policy values that are parsed from a deployment document
// or written into a checkpoint. A Grant is the document shape itself — there is no
// intermediate DTO — so every field of these types must carry both tags.
var documentTypes = []any{
	policy.Grant{},
	policy.Files{},
	policy.ToolSet{},
	policy.Budget{},
	policy.Delegation{},
	policy.Limits{},
	policy.ExecPolicy{},
	policy.Resources{},
	policy.EnvPolicy{},
	policy.NetPolicy{},
	policy.NetRule{},
	policy.MITMPolicy{},
	policy.Change{},
}

// TestEveryDocumentFieldIsTagged is the mechanical half of "a Grant is the
// document type": a field without a tag would be serialized under its Go name,
// which silently enters the deployment format and the digest.
func TestEveryDocumentFieldIsTagged(t *testing.T) {
	for _, value := range documentTypes {
		rt := reflect.TypeOf(value)
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if !field.IsExported() {
				continue
			}
			for _, tag := range []string{"json", "yaml"} {
				if field.Tag.Get(tag) == "" {
					t.Errorf("%s.%s has no %q tag", rt.Name(), field.Name, tag)
				}
			}
		}
	}
}

// TestEnumsAreTextInBothEncoders keeps the two enums out of the document as
// integers. A `mode: 1` in a deployment file is neither readable nor safe to
// reorder, and the two loaders (JSON semantics and YAML) look for different
// interfaces, so both have to be implemented.
func TestEnumsAreTextInBothEncoders(t *testing.T) {
	modes := map[policy.NetMode]string{
		policy.NetDefault:   "default",
		policy.NetDenyAll:   "deny_all",
		policy.NetAllowList: "allow_list",
		policy.NetProxy:     "proxy",
	}
	for mode, want := range modes {
		data, err := json.Marshal(mode)
		if err != nil {
			t.Fatalf("marshal %v: %v", want, err)
		}
		if string(data) != `"`+want+`"` {
			t.Fatalf("NetMode(%d) marshalled as %s, want %q", int(mode), data, want)
		}
		var back policy.NetMode
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", data, err)
		}
		if back != mode {
			t.Fatalf("round trip of %s landed on %d", data, int(back))
		}
		if _, ok := any(mode).(encoding.TextMarshaler); !ok {
			t.Fatalf("NetMode(%d) does not implement encoding.TextMarshaler; YAML would write an integer", int(mode))
		}
	}

	actions := map[policy.NetAction]string{policy.NetDeny: "deny", policy.NetAllow: "allow"}
	for action, want := range actions {
		data, err := json.Marshal(action)
		if err != nil {
			t.Fatalf("marshal %v: %v", want, err)
		}
		if string(data) != `"`+want+`"` {
			t.Fatalf("NetAction(%d) marshalled as %s, want %q", int(action), data, want)
		}
		if _, ok := any(action).(encoding.TextMarshaler); !ok {
			t.Fatalf("NetAction(%d) does not implement encoding.TextMarshaler", int(action))
		}
	}
}

func TestUnknownEnumValuesAreRefused(t *testing.T) {
	var mode policy.NetMode
	if err := json.Unmarshal([]byte(`"sideways"`), &mode); err == nil {
		t.Fatal("an unknown net mode was accepted")
	}
	var action policy.NetAction
	if err := json.Unmarshal([]byte(`"maybe"`), &action); err == nil {
		t.Fatal("an unknown net action was accepted")
	}
	// An integer would mean the document format drifted to the Go representation.
	if err := json.Unmarshal([]byte(`1`), &mode); err == nil {
		t.Fatal("an integer net mode was accepted")
	}
}

// TestGrantSerializesUnderItsDocumentNames pins the wire names, because the
// marshalled form is also what Digest hashes: renaming a tag changes every
// session.grant.id.
func TestGrantSerializesUnderItsDocumentNames(t *testing.T) {
	grant := policy.Grant{
		Sandbox: "locked",
		Files:   policy.Files{Read: []string{pathtest.Abs("srv/w")}, Write: []string{pathtest.Abs("srv/w/out")}},
		Net: policy.NetPolicy{
			Mode:       policy.NetAllowList,
			AllowHosts: []string{"api.example.com:443"},
			Rules:      []policy.NetRule{{Action: policy.NetAllow, Host: "api.example.com", Port: 443}},
			MITM:       &policy.MITMPolicy{Enabled: true, MaxBodyBytes: 1024},
		},
		Exec:      policy.ExecPolicy{Timeout: policy.Duration(10 * time.Second), Env: policy.EnvPolicy{Allow: []string{"PATH"}}, Shell: pathtest.Abs("bin/sh")},
		Resources: policy.Resources{CPUMillicores: 500, MemoryBytes: 1 << 20, MaxOutputBytes: 1 << 16},
		Models:    []model.ModelRef{{ID: model.ModelID{Provider: "openai", Name: "gpt-4o"}}},
		Tools: policy.ToolSet{
			Allow:    []string{"read_file"},
			Exposure: map[string]policy.Exposure{"read_file": policy.ExposureDeferred},
			Budget:   policy.Budget{MaxDefinitions: 8, MaxBytes: 2048},
		},
		Delegation: &policy.Delegation{Targets: []string{"other"}, Modes: []policy.DelegationMode{policy.DelegationSync}, MaxDepth: 1},
	}

	payload, err := json.Marshal(grant)
	if err != nil {
		t.Fatalf("marshal grant: %v", err)
	}
	text := string(payload)

	for _, key := range []string{
		`"sandbox"`, `"files"`, `"read"`, `"write"`,
		`"net"`, `"mode":"allow_list"`, `"allow_hosts"`, `"rules"`, `"action":"allow"`, `"port"`, `"mitm"`, `"max_body_bytes"`,
		`"exec"`, `"timeout":"10s"`, `"env"`, `"allow"`, `"shell"`,
		`"resources"`, `"cpu_millicores"`, `"memory_bytes"`, `"max_output_bytes"`,
		`"models"`, `"provider"`, `"name"`,
		`"tools"`, `"exposure"`, `"budget"`, `"max_definitions"`, `"max_bytes"`,
		`"delegation"`, `"targets"`, `"modes"`, `"max_depth"`,
	} {
		if !strings.Contains(text, key) {
			t.Errorf("marshalled grant is missing %s\n%s", key, text)
		}
	}

	// An unset bool is omitted rather than written as false, so the one field
	// that expresses "no writes" is asserted on its own.
	readonly, err := json.Marshal(policy.Files{ReadOnly: true})
	if err != nil {
		t.Fatalf("marshal readonly files: %v", err)
	}
	if !strings.Contains(string(readonly), `"readonly":true`) {
		t.Fatalf("readonly files marshalled as %s", readonly)
	}

	var back policy.Grant
	if err := json.Unmarshal(payload, &back); err != nil {
		t.Fatalf("unmarshal grant: %v", err)
	}
	if back.Digest() != grant.Digest() {
		t.Fatal("a JSON round trip changed the grant's identity")
	}
	if back.Exec.Timeout != grant.Exec.Timeout {
		t.Fatalf("durations did not round-trip: %v", back.Exec.Timeout)
	}
}

// TestDigestCoversTheWholeDocument is the reason the tags matter: the digest is
// the marshalled normalized grant, so every permission field has to reach it.
func TestDigestCoversTheWholeDocument(t *testing.T) {
	base := policy.Grant{Files: policy.Files{ReadOnly: true}}
	for name, change := range map[string]func(*policy.Grant){
		"allow_hosts": func(g *policy.Grant) { g.Net.AllowHosts = []string{"a.example.com"} },
		"rules":       func(g *policy.Grant) { g.Net.Rules = []policy.NetRule{{Action: policy.NetAllow, Host: "a"}} },
		"mitm":        func(g *policy.Grant) { g.Net.MITM = &policy.MITMPolicy{Enabled: true} },
		"unix":        func(g *policy.Grant) { g.Net.UnixSockets = []string{"/run/x.sock"} },
		"proxy":       func(g *policy.Grant) { g.Net = policy.NetPolicy{Mode: policy.NetProxy, Proxy: "http://p:8080"} },
		"env":         func(g *policy.Grant) { g.Exec.Env.Inject = map[string]string{"A": "1"} },
		"shell":       func(g *policy.Grant) { g.Exec.Shell = pathtest.Abs("bin/zsh") },
		"max_output":  func(g *policy.Grant) { g.Resources.MaxOutputBytes = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			change(&changed)
			if changed.Digest() == base.Digest() {
				t.Fatalf("changing %s did not change the digest", name)
			}
		})
	}
}

// TestNetNormalizedSortsUnorderedLists makes "the same policy written two ways"
// one digest, while leaving the ordered rule list alone.
func TestNetNormalizedSortsUnorderedLists(t *testing.T) {
	a, err := policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"b", "a"}}.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	b, err := policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"a", "b", "a"}}.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("host order changed the policy:\n%+v\n%+v", a, b)
	}

	ordered, err := policy.NetPolicy{
		Mode: policy.NetAllowList,
		Rules: []policy.NetRule{
			{Action: policy.NetDeny, Host: "evil.example"},
			{Action: policy.NetAllow, Host: "evil.example"},
		},
	}.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	if ordered.Rules[0].Action != policy.NetDeny {
		t.Fatal("rule order was normalized away; rules are matched in order")
	}
}

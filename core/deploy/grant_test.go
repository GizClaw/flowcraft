package deploy_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
)

// capsOf is what a process built from the shared test plan can carry.
func capsOf() deploy.Capabilities {
	return deploy.Capabilities{
		Backends: []string{"local"},
		Models:   []string{"openai/gpt-5"},
	}
}

// TestValidateGrantForCapsIsTheDocumentVersusProcessCheck covers the check that moved from install
// time to open time: the document no longer states authority, so the question "can this process
// carry what the caller asks for" is answered when a session is opened.
func TestValidateGrantForCapsIsTheDocumentVersusProcessCheck(t *testing.T) {
	cases := map[string]struct {
		grant   policy.Grant
		mention string
	}{
		"a backend this process does not run": {
			grant:   policy.Grant{Sandbox: "bwrap"},
			mention: "does not configure",
		},
		"a model this process does not serve": {
			grant:   policy.Grant{Models: []policy.ModelGrant{{ModelRef: model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-9"}}}}},
			mention: "does not serve",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := deploy.ValidateGrantForCaps("helper", tc.grant, capsOf())
			if err == nil || !errdefs.IsNotAvailable(err) {
				t.Fatalf("err = %v, want NotAvailable", err)
			}
			if !strings.Contains(err.Error(), "helper") || !strings.Contains(err.Error(), tc.mention) {
				t.Fatalf("error %q does not name the agent and %q", err, tc.mention)
			}
		})
	}
}

// TestValidateGrantForCapsAcceptsWhatTheProcessCarries is the accepting side, including the two
// empty defaults: no sandbox means the runtime's own, and no model list means every model.
func TestValidateGrantForCapsAcceptsWhatTheProcessCarries(t *testing.T) {
	for name, grant := range map[string]policy.Grant{
		"unconstrained": {},
		"a known backend and model": {
			Sandbox: "local",
			Models:  []policy.ModelGrant{{ModelRef: model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-5"}}}},
		},
	} {
		if err := deploy.ValidateGrantForCaps("helper", grant, capsOf()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

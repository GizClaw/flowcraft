package policy_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
)

// BenchmarkGrantNormalized is the per-session cost of turning a caller's
// authority into the value the host validates and binds. It runs once per
// session, so a regression here costs a session's first byte rather than every
// call.
func BenchmarkGrantNormalized(b *testing.B) {
	authority := policy.Grant{
		Files:  policy.Files{Read: []string{pathtest.Abs("srv/work")}, Write: []string{pathtest.Abs("srv/work/out")}},
		Models: []policy.ModelGrant{{ModelRef: model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-4o"}}}, {ModelRef: model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-4o-mini"}}}},
		Tools: policy.ToolSet{
			Allow:  []string{"read_file", "write_file", "exec", "grep"},
			Budget: policy.Budget{MaxDefinitions: 16, MaxBytes: 8192},
		},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := authority.Normalized(); err != nil {
			b.Fatalf("Normalized: %v", err)
		}
	}
}

// BenchmarkGrantDigest is the per-open identity cost: the value a span renders
// and a checkpoint compares.
func BenchmarkGrantDigest(b *testing.B) {
	authority := policy.Grant{
		Files:  policy.Files{Read: []string{pathtest.Abs("srv/work")}, Write: []string{pathtest.Abs("srv/work/out")}},
		Models: []policy.ModelGrant{{ModelRef: model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-4o"}}}},
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = authority.Digest()
	}
}

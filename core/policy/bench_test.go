package policy_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
)

// BenchmarkGrantNarrow is the per-session cost of producing a bind from an
// authority: a handful of subset checks over models and tools. It runs once per
// session, so a regression here costs a session's first byte rather than every call.
func BenchmarkGrantNarrow(b *testing.B) {
	authority := policy.Grant{
		Files:  policy.Files{Read: []string{pathtest.Abs("srv/work")}, Write: []string{pathtest.Abs("srv/work/out")}},
		Models: []model.ModelRef{{ID: model.ModelID{Provider: "openai", Name: "gpt-4o"}}, {ID: model.ModelID{Provider: "openai", Name: "gpt-4o-mini"}}},
		Tools: policy.ToolSet{
			Allow:  []string{"read_file", "write_file", "exec", "grep"},
			Budget: policy.Budget{MaxDefinitions: 16, MaxBytes: 8192},
		},
		Exec:   policy.ExecPolicy{Timeout: policy.Duration(30 * time.Second)},
		Limits: policy.Limits{Idle: policy.Duration(time.Minute), MaxSessions: 8},
	}
	request := policy.Request{
		Models: []model.ModelRef{{ID: model.ModelID{Provider: "openai", Name: "gpt-4o"}}},
		Tools:  &policy.ToolSet{Allow: []string{"read_file", "grep"}},
		Budget: &policy.Budget{MaxDefinitions: 8},
	}
	if _, err := authority.Narrow(request); err != nil {
		b.Fatalf("Narrow: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := authority.Narrow(request); err != nil {
			b.Fatalf("Narrow: %v", err)
		}
	}
}

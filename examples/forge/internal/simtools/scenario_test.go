package simtools_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/examples/forge/internal/simtools"
	"github.com/GizClaw/flowcraft/sdk/tool"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
)

func TestScenarioToolsDeclareAllSimtoolsAsBuiltin(t *testing.T) {
	registry := tool.NewRegistry()
	simtools.Register(registry, new(atomic.Int64))

	builder := toolconfig.NewBuilder(toolconfig.Deps{})
	for _, name := range registry.Names() {
		registered, ok := registry.Get(name)
		if !ok {
			t.Fatalf("builtin %q missing from registry", name)
		}
		builder.RegisterBuiltin(registered)
	}

	scenarios := []string{
		"../../scenarios/personas/tom/tools.yaml",
		"../../scenarios/raids/werewolf/tools.yaml",
		"../../scenarios/raids/multi_role_storyteller/tools.yaml",
	}
	for _, path := range scenarios {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			doc, err := toolconfig.Parse(data)
			if err != nil {
				t.Fatalf("Parse(%s): %v", path, err)
			}
			assembly, err := builder.Build(context.Background(), doc)
			if err != nil {
				t.Fatalf("Build(%s): %v", path, err)
			}
			t.Cleanup(func() { _ = assembly.Close() })
			for _, name := range registry.Names() {
				if _, ok := assembly.Catalog.Get(name); !ok {
					t.Errorf("%s: builtin tool %q missing from catalog", path, name)
				}
			}
		})
	}
}

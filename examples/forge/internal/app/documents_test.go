package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
)

// The shipped scenarios are the only documents that matter to this binary, so they are loaded here:
// a field renamed in the platform breaks this test instead of the first person to run forge.
func TestTheShippedScenariosLoad(t *testing.T) {
	for _, dir := range []string{
		filepath.Join("..", "..", "scenarios", "raids", "storyteller"),
		filepath.Join("..", "..", "scenarios", "raids", "werewolf"),
	} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			abs, err := filepath.Abs(dir)
			if err != nil {
				t.Fatalf("abs: %v", err)
			}
			// A key the documents need, so expansion is exercised rather than skipped.
			t.Setenv("OPENAI_API_KEY", "test-key")
			options := workspaceOptions(abs)
			plan, err := process(context.Background(), impls(), abs, options)
			if err != nil {
				t.Fatalf("process: %v", err)
			}
			if len(plan.Backends) == 0 {
				t.Fatal("the process document declared no backend")
			}
			// The capabilities are what a built resource reports; here the document's own declaration
			// stands in for it, because this test builds no process.
			caps := deploy.Capabilities{
				Backends: plan.BackendNames(),
				Models:   []string{"openai/gpt-6.1-sol"},
			}
			specs, err := deployments(context.Background(), impls(), abs, options, caps)
			if err != nil {
				t.Fatalf("deployments: %v", err)
			}
			if len(specs) < 2 {
				t.Fatalf("agents = %d, want the table", len(specs))
			}
			for _, spec := range specs {
				if spec.Program.Impl != "graph" {
					t.Errorf("agent %q program = %q", spec.Name, spec.Program.Impl)
				}
				if spec.Grant.Sandbox != "local" {
					t.Errorf("agent %q sandbox = %q", spec.Name, spec.Grant.Sandbox)
				}
			}
		})
	}
}

// A workspace whose key is unset says which key, once, before anything is built.
func TestAMissingKeyIsNamedOnce(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "scenarios", "raids", "werewolf"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	os.Unsetenv("OPENAI_API_KEY")
	missing, err := requiredEnvironment(context.Background(), dir)
	if err != nil {
		t.Fatalf("requiredEnvironment: %v", err)
	}
	if len(missing) != 1 || missing[0] != "OPENAI_API_KEY" {
		t.Fatalf("missing = %v, want the one key the documents name", missing)
	}
}

package exec_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
	"github.com/GizClaw/flowcraft/sdkx/tool/exec"
)

func builtinDoc(tools ...string) string {
	quoted := make([]string, 0, len(tools))
	for _, name := range tools {
		quoted = append(quoted, `"`+name+`"`)
	}
	return `{"version":"v1","sources":[{"kind":"builtin","spec":{"tools":[` +
		strings.Join(quoted, ",") + `]}}]}`
}

func TestRegisterBuiltin_ExecRunsThroughSandboxDep(t *testing.T) {
	builder := toolconfig.NewBuilder(toolconfig.Deps{})
	exec.RegisterBuiltin(builder)
	runner := &fakeRunner{
		retResult: &sandbox.ExecResult{ExitCode: 0, Stdout: "hello"},
	}
	value, err := builder.New(context.Background(), sdkconfig.Input{
		Resolve:  resolveLiteral(t),
		Settings: literalSettings(t, builtinDoc("exec")),
		Deps:     map[string]any{toolconfig.DepSandbox: runner},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assembly := value.(*toolconfig.Assembly)
	execTool, ok := assembly.Catalog.Get(exec.Name)
	if !ok {
		t.Fatal("exec missing from catalog")
	}
	out, err := execTool.Execute(
		context.Background(),
		`{"command":"ls","args":["-la"]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("Execute output = %q, want runner result", out)
	}
	if runner.gotCmd != "ls" {
		t.Fatalf("runner command = %q, want ls", runner.gotCmd)
	}
}

func TestRegisterBuiltin_SessionWithProcessManager(t *testing.T) {
	builder := toolconfig.NewBuilder(toolconfig.Deps{})
	exec.RegisterBuiltin(builder)
	value, err := builder.New(context.Background(), sdkconfig.Input{
		Resolve:  resolveLiteral(t),
		Settings: literalSettings(t, builtinDoc("exec", "exec_session")),
		Deps: map[string]any{
			toolconfig.DepSandbox: sandbox.NewLocalRunner(t.TempDir()),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assembly := value.(*toolconfig.Assembly)
	sessionTool, ok := assembly.Catalog.Get(exec.SessionName)
	if !ok {
		t.Fatal("exec_session missing from catalog")
	}
	t.Cleanup(func() {
		_ = sessionTool.(interface{ Close() error }).Close()
	})
}

func TestRegisterBuiltin_MissingSandboxDepFailsClosed(t *testing.T) {
	builder := toolconfig.NewBuilder(toolconfig.Deps{})
	exec.RegisterBuiltin(builder)
	_, err := builder.New(context.Background(), sdkconfig.Input{
		Resolve:  resolveLiteral(t),
		Settings: literalSettings(t, builtinDoc("exec")),
	})
	if err == nil || !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("New error = %v, want missing sandbox dep validation", err)
	}
}

func TestRegisterBuiltin_WrongDepTypeFailsClosed(t *testing.T) {
	builder := toolconfig.NewBuilder(toolconfig.Deps{})
	exec.RegisterBuiltin(builder)
	_, err := builder.New(context.Background(), sdkconfig.Input{
		Resolve:  resolveLiteral(t),
		Settings: literalSettings(t, builtinDoc("exec")),
		Deps:     map[string]any{toolconfig.DepSandbox: "not-a-runner"},
	})
	if err == nil || !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "sandbox.Runner") {
		t.Fatalf("New error = %v, want wrong dep type validation", err)
	}
}

func TestRegisterBuiltin_SessionRequiresProcessManager(t *testing.T) {
	builder := toolconfig.NewBuilder(toolconfig.Deps{})
	exec.RegisterBuiltin(builder)
	_, err := builder.New(context.Background(), sdkconfig.Input{
		Resolve:  resolveLiteral(t),
		Settings: literalSettings(t, builtinDoc("exec_session")),
		Deps: map[string]any{
			toolconfig.DepSandbox: &fakeRunner{},
		},
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("New error = %v, want NotAvailable for runner without sessions", err)
	}
}

func literalSettings(t *testing.T, doc string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal literal settings: %v", err)
	}
	return json.RawMessage(raw)
}

func resolveLiteral(t *testing.T) func(context.Context, sdkconfig.Source) ([]byte, error) {
	t.Helper()
	return func(ctx context.Context, src sdkconfig.Source) ([]byte, error) {
		return sdkconfig.NewLoader().Load(ctx, src)
	}
}

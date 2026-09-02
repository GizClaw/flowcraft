package workspace_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

func TestRegister(t *testing.T) {
	reg := resource.NewRegistry()
	if err := workspace.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	factory, ok := reg.Lookup("workspace.Workspace", "local")
	if !ok {
		t.Fatal("workspace.Workspace/local factory not registered")
	}
	root, err := json.Marshal(t.TempDir())
	if err != nil {
		t.Fatalf("marshal root: %v", err)
	}
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"root": ` + string(root) + `}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	closeWorkspaceValue(t, value)
	if _, ok := value.(*workspace.LocalWorkspace); !ok {
		t.Fatalf("New returned %T, want *workspace.LocalWorkspace", value)
	}
}

func TestFactoryRequiresRoot(t *testing.T) {
	reg := resource.NewRegistry()
	if err := workspace.Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup("workspace.Workspace", "local")
	if _, err := factory.New(context.Background(), resource.Input{}); err == nil {
		t.Fatal("New unexpectedly accepted missing root")
	}
}

func TestFactoryScopedDisabled(t *testing.T) {
	reg := resource.NewRegistry()
	if err := workspace.Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup("workspace.Workspace", "local")
	root, err := json.Marshal(t.TempDir())
	if err != nil {
		t.Fatalf("marshal root: %v", err)
	}
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"root": ` + string(root) + `,
			"scoped": {"enabled": false, "deny_read": ["secret/**"]}}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	closeWorkspaceValue(t, value)
	if _, ok := value.(*workspace.LocalWorkspace); !ok {
		t.Fatalf("New returned %T, want *workspace.LocalWorkspace", value)
	}
}

func TestFactoryScopedEnabled(t *testing.T) {
	reg := resource.NewRegistry()
	if err := workspace.Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup("workspace.Workspace", "local")
	root, err := json.Marshal(t.TempDir())
	if err != nil {
		t.Fatalf("marshal root: %v", err)
	}
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"root": ` + string(root) + `,
			"scoped": {"enabled": true, "allow_write": ["public/**"]}}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	closeWorkspaceValue(t, value)
	scoped, ok := value.(*workspace.ScopedWorkspace)
	if !ok {
		t.Fatalf("New returned %T, want *workspace.ScopedWorkspace", value)
	}
	ctx := context.Background()
	if err := scoped.Write(ctx, "secret/key.txt", []byte("x")); !errors.Is(err, workspace.ErrAccessDenied) {
		t.Fatalf("write to denied path error = %v, want ErrAccessDenied", err)
	}
	if err := scoped.Write(ctx, "public/ok.txt", []byte("x")); err != nil {
		t.Fatalf("write to allowed path: %v", err)
	}
}

func TestFactoryRelativeRootUsesLoaderBaseDir(t *testing.T) {
	base := t.TempDir()
	reg := resource.NewRegistry()
	if err := workspace.Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup("workspace.Workspace", "local")
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"root": "./ws"}`),
		Loader:   resource.NewLoader(resource.WithBaseDir(base)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	closeWorkspaceValue(t, value)
	local, ok := value.(*workspace.LocalWorkspace)
	if !ok {
		t.Fatalf("New returned %T, want *workspace.LocalWorkspace", value)
	}
	want := filepath.Join(base, "ws")
	resolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	if local.Root() != resolved {
		t.Fatalf("root = %q, want %q", local.Root(), resolved)
	}
}

func TestFactoryExpandsSettingsRefs(t *testing.T) {
	envRoot := t.TempDir()
	base := t.TempDir()
	home := t.TempDir()
	t.Setenv("FLOWCRAFT_TEST_WS_ROOT", envRoot)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	reg := resource.NewRegistry()
	if err := workspace.Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup("workspace.Workspace", "local")

	for _, tc := range []struct {
		name string
		root string
		want string
	}{
		{"env", "${env:FLOWCRAFT_TEST_WS_ROOT}", envRoot},
		{"base", "${base}", base},
		{"base rel", "${base:ws}", filepath.Join(base, "ws")},
		{"home tilde", "~/ws", filepath.Join(home, "ws")},
		{"home ref", "${home:ws}", filepath.Join(home, "ws")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expanded, err := resource.Expand(context.Background(),
				[]byte(`{"root": "`+tc.root+`"}`),
				resource.ExpandEnv(), resource.ExpandHome(), resource.ExpandBase(base))
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			value, err := factory.New(context.Background(), resource.Input{
				Settings: expanded,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			closeWorkspaceValue(t, value)
			local, ok := value.(*workspace.LocalWorkspace)
			if !ok {
				t.Fatalf("New returned %T, want *workspace.LocalWorkspace", value)
			}
			want, err := filepath.EvalSymlinks(tc.want)
			if err != nil {
				t.Fatalf("eval symlinks: %v", err)
			}
			if local.Root() != want {
				t.Fatalf("root = %q, want %q", local.Root(), want)
			}
		})
	}
}

func closeWorkspaceValue(t *testing.T, value any) {
	t.Helper()
	t.Cleanup(func() {
		if closer, ok := value.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})
}

func TestFactoryExpansionErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
	}{
		{"unset env", `{"root": "${env:FLOWCRAFT_TEST_WS_UNSET}"}`},
		{"unknown ref", `{"root": "${unknown}"}`},
		{"base without base dir", `{"root": "${base}"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resource.Expand(context.Background(),
				[]byte(tc.settings),
				resource.ExpandEnv(), resource.ExpandHome(), resource.ExpandBase(""))
			if !errdefs.IsValidation(err) {
				t.Fatalf("Expand error = %v, want validation error", err)
			}
		})
	}
}

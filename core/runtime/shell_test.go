package runtime

import (
	"fmt"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/goccy/go-json"
)

// TestLoadLayersDecodesPinnedShell pins the written key, not the struct field:
// tools.exec.shell is the name a deployment uses, and the strict decoder is what
// rejects it when the tag and the document disagree. A Windows path is used
// because that is the shape this field exists for, and the canonical form has to
// keep its backslashes.
func TestLoadLayersDecodesPinnedShell(t *testing.T) {
	root := pathtest.Abs("tmp")
	body := fmt.Sprintf(`
version: 1
sandbox: {backend: local}
files: {roots: ['%s']}
tools:
  exec:
    shell:
      name: pwsh
      path: C:\Program Files\PowerShell\7\pwsh.exe
      args: [-NoProfile, -Command]
`, root)
	layer := config.Layer{Priority: 0, Name: "test", Source: config.Bytes([]byte(body))}
	cfg, err := LoadLayers(t.Context(), []config.Layer{layer})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	shell := cfg.Tools.shell()
	if shell == nil {
		t.Fatalf("tools.exec.shell did not decode; config = %+v", cfg.Tools)
	}
	if shell.Name != "pwsh" || shell.Path != `C:\Program Files\PowerShell\7\pwsh.exe` {
		t.Fatalf("shell = %+v, want the file's name and path", shell)
	}
	if len(shell.Args) != 2 {
		t.Fatalf("args = %q, want the two the document lists", shell.Args)
	}
}

// TestUnconfiguredShellKeepsTheDigest pins the omitempty on tools.exec. Adding a
// configuration field must not change the digest of a deployment that does not
// use it: every stored checkpoint of every existing deployment would stop
// resuming, for no change. The assertion is on the canonical form, because that
// is what the digest hashes.
func TestUnconfiguredShellKeepsTheDigest(t *testing.T) {
	base := validConfig(pathtest.Abs("tmp"))
	// An exec block that pins nothing is the same configuration as no block: a
	// deployment that writes `exec: {}` must not lose its checkpoints either.
	empty := base
	empty.Tools.Exec = &ExecConfig{}
	for name, cfg := range map[string]Config{"no block": base, "empty block": empty} {
		body, err := json.Marshal(cfg.redacted())
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if strings.Contains(string(body), `"exec":`) {
			t.Fatalf("%s: an unconfigured exec block reaches the digest: %s", name, body)
		}
	}
	if base.Digest() != empty.Digest() {
		t.Errorf("an empty exec block changed the digest: %s != %s", base.Digest(), empty.Digest())
	}
}

// TestPinnedShellChangesTheDigest keeps the deployment's identity honest: a
// session that resumes under a different shell would run the model's next command
// line through a different grammar than the transcript describes.
func TestPinnedShellChangesTheDigest(t *testing.T) {
	base := validConfig(pathtest.Abs("tmp"))
	pinned := base
	pinned.Tools.Exec = &ExecConfig{Shell: &ShellConfig{Name: "bash", Path: "/bin/bash"}}
	if base.Digest() == pinned.Digest() {
		t.Fatal("the digest ignored a pinned shell")
	}
}

// TestRemoteShellKeepsItsName pins the cross-boundary rule: the probed path is a
// fact about the machine that built the runtime, so a remote backend receives the
// name and lets the far side resolve it. Sending a local path would fail on every
// call whenever the two hosts install the shell differently.
func TestRemoteShellKeepsItsName(t *testing.T) {
	resolved := sandbox.ShellSpec{
		Name: "pwsh",
		Path: `C:\Program Files\PowerShell\7\pwsh.exe`,
		Args: []string{"-NoProfile", "-NonInteractive", "-Command"},
	}
	remote := shellForTransport(resolved, BackendRemote)
	if remote.Path != "pwsh" {
		t.Fatalf("remote path = %q, want the bare name the far side resolves", remote.Path)
	}
	if remote.Name != resolved.Name || len(remote.Args) != len(resolved.Args) {
		t.Fatalf("remote spec = %+v, want the name and argv kept", remote)
	}
	if local := shellForTransport(resolved, BackendLocal); local.Path != resolved.Path {
		t.Fatalf("local path = %q, want the resolved executable", local.Path)
	}
}

// TestPinnedShellNamesTheTool follows the whole path — config, validation, tool
// construction, execution — because each step could drop the pin without failing:
// the tool would keep its probed name and the deployment would look configured.
func TestPinnedShellNamesTheTool(t *testing.T) {
	// The pinned shell is the one this host can start without assuming anything
	// the deployment would be allowed to pin: a POSIX lane pins sh, and a Windows
	// lane pins cmd, the shell every Windows install has.
	pinned, probed, command := "sh", "bash", "echo pinned"
	if goruntime.GOOS == "windows" {
		pinned, probed, command = "cmd", "pwsh", "echo pinned"
	}
	cfg := localConfig(t)
	cfg.Tools.Exec = &ExecConfig{Shell: &ShellConfig{Name: pinned, Path: pinned}}
	rt, err := newRuntime(t, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if _, ok := rt.Tools().Get(pinned); !ok {
		t.Fatalf("tools = %v, want the pinned shell's tool", rt.Tools().Names())
	}
	if _, ok := rt.Tools().Get(probed); ok {
		t.Fatalf("tools = %v, want the probed shell replaced, not kept", rt.Tools().Names())
	}

	call, err := message.NewToolCall("call-1", pinned, map[string]any{"command": command})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	result := rt.Assembly().Execute(rt.Context(t.Context()), call)
	if result.IsError {
		t.Fatalf("exec result = %q", result.Content.Text())
	}
	if !strings.Contains(result.Content.Text(), "pinned") {
		t.Fatalf("exec output = %q, want the command's output", result.Content.Text())
	}
}

package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft/plugin/protocol"
	"github.com/GizClaw/flowcraft/craft/plugin/server"
)

// The knobs the re-executed helper reads. They share one prefix so a
// single variable tells a helper process from a test run.
const (
	helperEnv    = "CRAFT_PLUGIN_HELPER"
	helperTools  = "CRAFT_PLUGIN_HELPER_TOOLS"
	helperExitMS = "CRAFT_PLUGIN_HELPER_EXIT_MS"
	helperNoWire = "CRAFT_PLUGIN_HELPER_NO_WIRE"
)

// helperDefaultTools is what the helper declares when the test named no
// tool set: a text result that is not JSON (craft wraps it), and the
// writes contract a plugin graph node consumes.
const helperDefaultTools = "echo,node_echo"

// TestPluginWireHelper is the plugin process the tests in this package
// start: this test binary re-executed, speaking the plugin half of the
// wire over the socket craft opened for it. Everything it varies by is an
// environment variable, because a plugin process learns its world from
// the environment craft hands it and nothing else.
func TestPluginWireHelper(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("plugin helper")
	}
	recordHelperPID(t)
	if raw := os.Getenv(helperExitMS); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms <= 0 {
			t.Fatalf("invalid %s %q", helperExitMS, raw)
		}
		time.AfterFunc(time.Duration(ms)*time.Millisecond, func() { os.Exit(0) })
	}
	if os.Getenv(helperNoWire) == "1" {
		// A process that is alive and says nothing: it is how the tests
		// reach the "not yet" half of an enable, where the plugin never
		// dials the socket and the wait runs out of time instead of
		// learning the plugin is broken.
		//
		// The wait is a loop over a timer rather than an empty select: a
		// goroutine blocked forever with nothing else runnable is what
		// the runtime reports as a deadlock, and the helper would exit
		// for a reason that has nothing to do with the wire.
		for {
			time.Sleep(time.Second)
		}
	}
	tools := strings.Split(os.Getenv(helperTools), ",")
	if len(tools) == 0 || os.Getenv(helperTools) == "" {
		tools = strings.Split(helperDefaultTools, ",")
	}
	handler := wireHelper{tools: tools}
	err := protocol.Main(context.Background(), handler, protocol.ClientOptions{
		PluginID:    os.Getenv(protocol.IDEnv),
		Version:     "0.1.0",
		Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("wire helper: %v", err)
	}
}

// wireHelper is the plugin the helper process serves: it declares the
// tools it was told to and answers them, so a test can drive a real
// connection without a real plugin.
type wireHelper struct{ tools []string }

// Declare publishes one full snapshot of the helper's tools.
func (h wireHelper) Declare(
	context.Context,
	*protocol.HelloResponse,
	protocol.HostClient,
) (*protocol.Declaration, error) {
	declaration := &protocol.Declaration{}
	for _, name := range h.tools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		declaration.Tools = append(declaration.Tools, &protocol.ToolDecl{
			Name:        name,
			Description: "helper tool " + name,
			InputSchema: []byte(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		})
	}
	return declaration, nil
}

// Call answers one invocation of the helper's tools.
func (h wireHelper) Call(
	_ context.Context,
	request *protocol.CallRequest,
) ([]byte, error) {
	var arguments struct {
		Text string `json:"text"`
	}
	if raw := request.GetArgumentsJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, errdefs.Validationf("helper: arguments: %v", err)
		}
	}
	switch request.GetTool() {
	case "echo":
		// Not JSON on purpose: a plugin's result is bytes, and craft
		// wraps one that is not JSON rather than failing the call.
		return []byte("echo:" + arguments.Text), nil
	case "node_echo":
		return json.Marshal(map[string]any{
			"writes": map[string]any{"result": "echo:" + arguments.Text},
		})
	case "fail":
		return nil, errdefs.Validationf("helper refuses %q", arguments.Text)
	default:
		return nil, errdefs.NotFoundf("helper has no tool %q", request.GetTool())
	}
}

// recordHelperPID appends the helper's pid to the plugin data directory,
// so a test can kill the process the way a crash would.
func recordHelperPID(t *testing.T) {
	t.Helper()
	dir := os.Getenv(protocol.DataDirEnv)
	if dir == "" {
		return
	}
	file, err := os.OpenFile(
		filepath.Join(dir, "pids"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open pid file: %v", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		t.Fatalf("record pid: %v", err)
	}
}

// helperEnvJSON is the run.env of every helper-backed manifest: the gate
// that turns the test binary into a plugin, plus whatever the test adds.
func helperEnvJSON(extra map[string]string) string {
	env := map[string]string{helperEnv: "1"}
	for key, value := range extra {
		env[key] = value
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// helperRunJSON is the run section of a helper-backed manifest.
func helperRunJSON(extra map[string]string) string {
	return `"run": {` +
		`"command": "./server", ` +
		`"args": ["-test.run=TestPluginWireHelper"], ` +
		`"env": ` + helperEnvJSON(extra) + `}`
}

// writeHelperCommand links this test binary into the plugin directory as
// the plugin's command, replacing the placeholder the contribution tree
// lays down.
func writeHelperCommand(t *testing.T, dir string) {
	t.Helper()
	if os.Getenv(helperEnv) == "1" {
		t.Skip("running as helper")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	target := filepath.Join(dir, "server")
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		t.Fatalf("replace placeholder: %v", err)
	}
	if err := os.Symlink(executable, target); err != nil {
		t.Fatalf("symlink helper: %v", err)
	}
}

// newWireServer builds the craft half of the wire for one test. The
// primitive set is empty on purpose: these tests are about the tool
// surface a plugin provides, not about what it may ask craft for.
func newWireServer(t *testing.T) *server.Server {
	t.Helper()
	srv, err := server.NewServer(server.ServerOptions{
		Registry:    server.NewRegistry(),
		HostVersion: "test",
	})
	if err != nil {
		t.Fatalf("server.NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// wireManifest is the manifest of a plugin that contributes nothing but
// what it declares over the wire.
func wireManifest(id string, permissions ...string) string {
	return wireManifestEnv(id, nil, permissions...)
}

// wireManifestEnv is wireManifest with extra environment for the helper:
// the knobs above are how one test says what kind of plugin it wants.
func wireManifestEnv(id string, env map[string]string, permissions ...string) string {
	granted := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		granted = append(granted, `"`+permission+`"`)
	}
	return `{
		"id": "` + id + `", "version": "0.1.0",
		"permissions": [` + strings.Join(granted, ", ") + `],
		` + helperRunJSON(env) + `
	}`
}

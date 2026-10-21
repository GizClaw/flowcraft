package craft

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft/plugin/protocol"
)

// The knobs the re-executed helper reads. They share one prefix so a single
// variable tells a helper process from a test run.
const (
	craftWireHelperEnv = "CRAFT_CRAFT_WIRE_HELPER"
	craftWireTools     = "CRAFT_CRAFT_WIRE_TOOLS"
	craftWireNoDial    = "CRAFT_CRAFT_WIRE_NO_DIAL"
)

// craftWireDefaultTools is what the helper declares when the test named no
// tool set: the writes contract a plugin node consumes.
const craftWireDefaultTools = "node_echo"

// observationsFile is where the helper records what its connection told it.
// It is the file the tests in this package assert on: a plugin process and
// craft share no memory, so what the plugin learned has to land on disk.
const observationsFile = "observations"

// TestCraftPluginWireHelper is the plugin process the tests in this package
// start: this test binary re-executed, speaking the plugin half of the wire
// over the socket craft opened for it.
func TestCraftPluginWireHelper(t *testing.T) {
	if os.Getenv(craftWireHelperEnv) != "1" {
		t.Skip("plugin helper")
	}
	if os.Getenv(craftWireNoDial) == "1" {
		// A process that is alive and says nothing: it is how the tests
		// reach the "not yet" half of an enable. It waits on a timer
		// rather than an empty select, which the runtime reports as a
		// deadlock and which would exit the helper for a reason that has
		// nothing to do with the wire.
		for {
			time.Sleep(time.Second)
		}
	}
	tools := strings.Split(os.Getenv(craftWireTools), ",")
	if os.Getenv(craftWireTools) == "" {
		tools = strings.Split(craftWireDefaultTools, ",")
	}
	handler := &craftWireHelper{tools: tools}
	if err := protocol.Main(context.Background(), handler, protocol.ClientOptions{
		PluginID:    os.Getenv(protocol.IDEnv),
		Version:     "0.1.0",
		Concurrency: 2,
	}); err != nil {
		t.Fatalf("wire helper: %v", err)
	}
}

// observation is one connection's view of what craft granted it, written by
// the helper before it declares anything.
type observation struct {
	Generation  uint64   `json:"generation"`
	Grants      []string `json:"grants"`
	Secret      string   `json:"secret,omitempty"`
	SecretError string   `json:"secret_error,omitempty"`
}

// craftWireHelper is the plugin the helper process serves: it declares the
// tools it was told to, answers the node contract, and records what its
// connection was granted.
type craftWireHelper struct{ tools []string }

// Declare records the connection's grants and tries one primitive, so a test
// can tell what this process is allowed to do rather than only what craft
// says it decided.
func (h *craftWireHelper) Declare(
	ctx context.Context,
	hello *protocol.HelloResponse,
	host protocol.HostClient,
) (*protocol.Declaration, error) {
	found := observation{
		Generation: hello.GetGeneration(),
		Grants:     append([]string(nil), hello.GetGrantedPermissions()...),
	}
	var response struct {
		Value string `json:"value"`
	}
	err := protocol.Invoke(ctx, host, "secret", "get",
		map[string]any{"name": "k"}, &response)
	if err != nil {
		found.SecretError = err.Error()
	} else {
		found.Secret = response.Value
	}
	if err := recordObservation(found); err != nil {
		return nil, err
	}
	declaration := &protocol.Declaration{}
	for _, name := range h.tools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		declaration.Tools = append(declaration.Tools, &protocol.ToolDecl{
			Name:        name,
			Description: "helper tool " + name,
			InputSchema: []byte(`{"type":"object","properties":{"node":{"type":"object"},"config":{"type":"object"},"text":{"type":"string"}}}`),
		})
	}
	return declaration, nil
}

// Call answers one invocation: the node writes contract for node_echo, and
// the text-result shape for echo.
func (h *craftWireHelper) Call(
	_ context.Context,
	request *protocol.CallRequest,
) ([]byte, error) {
	var arguments struct {
		Node struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"node"`
		Config map[string]any `json:"config"`
		Text   string         `json:"text"`
	}
	if raw := request.GetArgumentsJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, errdefs.Validationf("helper: arguments: %v", err)
		}
	}
	switch request.GetTool() {
	case "node_echo":
		writes := map[string]any{
			"result":   "echo:" + arguments.Node.ID,
			"nodeType": arguments.Node.Type,
		}
		for key, value := range arguments.Config {
			writes[key] = value
		}
		return json.Marshal(map[string]any{"writes": writes})
	case "echo":
		// Not JSON on purpose: a plugin's result is bytes, and craft
		// wraps one that is not JSON rather than failing the call.
		return []byte("echo:" + arguments.Text), nil
	default:
		return nil, errdefs.NotFoundf("helper has no tool %q", request.GetTool())
	}
}

// recordObservation appends one line to the helper's observations file.
func recordObservation(found observation) error {
	dir := os.Getenv(protocol.DataDirEnv)
	if dir == "" {
		return errdefs.Internalf("plugin helper: %s is not set", protocol.DataDirEnv)
	}
	payload, err := json.Marshal(found)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(
		filepath.Join(dir, observationsFile),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = file.WriteString(string(payload) + "\n")
	return err
}

// writeWirePlugin lays down one plugin whose process is the helper, and
// returns the plugin directory.
func writeWirePlugin(
	t *testing.T,
	root, id, version string,
	extraEnv map[string]string,
	permissions ...string,
) string {
	t.Helper()
	if os.Getenv(craftWireHelperEnv) == "1" {
		t.Skip("running as helper")
	}
	dir := newWirePluginDir(t, root, id)
	if err := os.Symlink(wireHelperExecutable(t), filepath.Join(dir, "server")); err != nil {
		t.Fatalf("symlink helper: %v", err)
	}
	writeWirePluginManifest(t, dir, id, version, extraEnv, permissions)
	return dir
}

// writeInstallableWirePlugin lays down one plugin the store can install,
// and returns its directory. The helper is copied rather than linked: an
// install refuses a symlink, and it is the store's own copy that a
// rollback restores.
func writeInstallableWirePlugin(
	t *testing.T,
	root, id, version string,
	permissions ...string,
) string {
	t.Helper()
	executable := wireHelperExecutable(t)
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	dir := newWirePluginDir(t, root, id)
	if err := os.WriteFile(filepath.Join(dir, "server"), data, 0o700); err != nil {
		t.Fatalf("copy helper: %v", err)
	}
	writeWirePluginManifest(t, dir, id, version, nil, permissions)
	return dir
}

// wireHelperExecutable is the test binary the helper is re-executed from.
func wireHelperExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	return executable
}

// newWirePluginDir creates one plugin directory under root.
func newWirePluginDir(t *testing.T, root, id string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir
}

// writeWirePluginManifest writes the manifest that makes the helper the
// plugin's process.
func writeWirePluginManifest(
	t *testing.T,
	dir, id, version string,
	extraEnv map[string]string,
	permissions []string,
) {
	t.Helper()
	env := map[string]string{craftWireHelperEnv: "1"}
	for key, value := range extraEnv {
		env[key] = value
	}
	encodedEnv, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal env: %v", err)
	}
	quoted := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		quoted = append(quoted, `"`+permission+`"`)
	}
	manifest := `{
		"id": "` + id + `", "version": "` + version + `",
		"permissions": [` + strings.Join(quoted, ", ") + `],
		"nodes": [{"type": "echo", "tool": "node_echo", "timeout": "10s"}],
		"run": {
			"command": "./server",
			"args": ["-test.run=TestCraftPluginWireHelper"],
			"env": ` + string(encodedEnv) + `
		}
	}`
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("manifest: %v", err)
	}
}

// readObservations returns every observation the helper recorded in one
// plugin's data directory, in the order the connections were made.
func readObservations(t *testing.T, dataDir, id string) []observation {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, id, observationsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read observations: %v", err)
	}
	var out []observation
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var found observation
		if err := json.Unmarshal([]byte(line), &found); err != nil {
			t.Fatalf("observation %q: %v", line, err)
		}
		out = append(out, found)
	}
	return out
}

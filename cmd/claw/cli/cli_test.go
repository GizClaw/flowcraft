package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceCreateCmdUsesConfigFlag(t *testing.T) {
	dir := t.TempDir()
	createWorkspaceFromConfig(t, "examples/raids/chat.yaml", dir)
	path := filepath.Join(dir, configFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("missing %s: %v", path, err)
	}
}

func TestWorkspaceCreateCmdUsesEmbeddedConfigPrefixWithoutExtension(t *testing.T) {
	dir := t.TempDir()
	createWorkspaceFromConfig(t, "chat", dir)
	if _, err := os.Stat(filepath.Join(dir, configFileName)); err != nil {
		t.Fatalf("missing config file: %v", err)
	}
}

func TestRunRejectsRemovedCommands(t *testing.T) {
	t.Setenv("CLAW_CONFIG_DIR", t.TempDir())
	for _, cmd := range []string{"chat", "auto", "run", "roundtrip", "debug-run", "test-script", "create", "examples"} {
		t.Run(cmd, func(t *testing.T) {
			err := Execute([]string{cmd})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("error = %v, want unknown command", err)
			}
		})
	}
}

func TestUsageShowsRunCommand(t *testing.T) {
	text := usage()
	if !strings.Contains(text, "claw test-auto --raid") {
		t.Fatalf("usage = %q, want claw test-auto command", text)
	}
	if !strings.Contains(text, "claw test -test") {
		t.Fatalf("usage = %q, want claw test command", text)
	}
	if !strings.Contains(text, "claw serve --workspace") {
		t.Fatalf("usage = %q, want claw serve command", text)
	}
	if !strings.Contains(text, "claw tui new") || !strings.Contains(text, "claw tui resume") {
		t.Fatalf("usage = %q, want tui commands", text)
	}
	if !strings.Contains(text, "claw config raid list") ||
		!strings.Contains(text, "claw config persona list") ||
		!strings.Contains(text, "claw config test list") {
		t.Fatalf("usage = %q, want config list commands", text)
	}
	if strings.Contains(text, "raid-config") || strings.Contains(text, "persona-config") {
		t.Fatalf("usage still contains old config suffix flags: %q", text)
	}
	if strings.Contains(text, "-out") || strings.Contains(text, "--out") {
		t.Fatalf("usage still contains output flags: %q", text)
	}
	if strings.Contains(text, "claw auto") {
		t.Fatalf("usage still contains old auto command: %q", text)
	}
	if strings.Contains(text, "claw run") {
		t.Fatalf("usage still exposes run compatibility alias: %q", text)
	}
	if strings.Contains(text, "roundtrip") || strings.Contains(text, "debug-run") {
		t.Fatalf("usage still exposes removed test command: %q", text)
	}
	if strings.Contains(text, "test-script") || strings.Contains(text, "script") {
		t.Fatalf("usage still exposes script naming: %q", text)
	}
	if strings.Contains(text, "list-examples") || strings.Contains(text, "configs/tests") {
		t.Fatalf("usage still exposes old config listing/test plural naming: %q", text)
	}
}

func TestServeHandlerExposesDebugOnlyRoutes(t *testing.T) {
	dir := t.TempDir()
	writeServeTestConfig(t, dir)

	handler, closeFn, err := serveHandler(dir)
	if err != nil {
		t.Fatalf("serveHandler: %v", err)
	}
	defer func() {
		if err := closeFn(); err != nil {
			t.Fatalf("close serve handler: %v", err)
		}
	}()
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET / status = %d, want 404", resp.StatusCode)
	}

	resp, err = http.Get(server.URL + "/debug/workspace")
	if err != nil {
		t.Fatalf("GET /debug/workspace: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /debug/workspace status = %d, want 200", resp.StatusCode)
	}
	var debug map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&debug); err != nil {
		t.Fatalf("decode debug workspace: %v", err)
	}
	agent, ok := debug["agent"].(map[string]any)
	if !ok || agent["id"] != "serve-test" {
		t.Fatalf("debug agent = %#v, want serve-test", debug["agent"])
	}

	resp, err = http.Get(server.URL + "/v1/webrtc/offer")
	if err != nil {
		t.Fatalf("GET /v1/webrtc/offer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/webrtc/offer status = %d, want 404", resp.StatusCode)
	}
}

func TestReadConfigSourceSupportsLocalFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "chat.yaml")
	raw, err := templateFS.ReadFile("examples/raids/chat.yaml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(source, raw, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	outDir := filepath.Join(dir, "workspace")
	if err := workspaceCreateCmd([]string{"--config", source, "--workspace", outDir}); err != nil {
		t.Fatalf("workspaceCreateCmd local config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, configFileName)); err != nil {
		t.Fatalf("missing config file: %v", err)
	}
}

func TestRunWithoutArgsPrintsHelp(t *testing.T) {
	t.Setenv("CLAW_CONFIG_DIR", t.TempDir())
	if err := Execute(nil); err != nil {
		t.Fatalf("Execute nil: %v", err)
	}
}

func TestOpenAppReadsCreatedConfig(t *testing.T) {
	dir := t.TempDir()
	createWorkspaceFromConfig(t, "examples/raids/chat.yaml", dir)
	setExampleEnv(t)
	app, err := openApp(dir)
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer app.Close()
	if got := app.Config().Models.Chat; got != "generate_model" {
		t.Fatalf("chat model alias = %q, want generate_model", got)
	}
}

func TestWorkspaceInspectReadsCreatedConfig(t *testing.T) {
	dir := t.TempDir()
	createWorkspaceFromConfig(t, "journey", dir)
	setExampleEnv(t)
	if err := workspaceInspectCmd([]string{"--workspace", dir}); err != nil {
		t.Fatalf("workspaceInspectCmd: %v", err)
	}
}

func TestConfigListCommands(t *testing.T) {
	var out strings.Builder
	if err := configCmdWithOutput([]string{"raid", "list"}, &out); err != nil {
		t.Fatalf("config raid list: %v", err)
	}
	raids, err := listRaids()
	if err != nil {
		t.Fatalf("listRaids: %v", err)
	}
	if !contains(raids, "journey") || !contains(raids, "chat") || !contains(raids, "func_chat") {
		t.Fatalf("raids = %v, want chat, journey, and func_chat", raids)
	}
	if !strings.Contains(out.String(), "journey") || !strings.Contains(out.String(), "chat") {
		t.Fatalf("config raid list output = %q, want journey and chat", out.String())
	}

	out.Reset()
	if err := configCmdWithOutput([]string{"persona", "list"}, &out); err != nil {
		t.Fatalf("config persona list: %v", err)
	}
	personas, err := listPersonas()
	if err != nil {
		t.Fatalf("listPersonas: %v", err)
	}
	if !contains(personas, "boy_14_Tom") {
		t.Fatalf("personas = %v, want boy_14_Tom", personas)
	}
	if !strings.Contains(out.String(), "boy_14_Tom") {
		t.Fatalf("config persona list output = %q, want boy_14_Tom", out.String())
	}

	out.Reset()
	if err := configCmdWithOutput([]string{"test", "list"}, &out); err != nil {
		t.Fatalf("config test list: %v", err)
	}
	tests, err := listTests()
	if err != nil {
		t.Fatalf("listTests: %v", err)
	}
	for _, want := range []string{
		"match_route/music_flow",
		"match_route/music_direct",
		"match_route/story_subject",
		"match_route/volume_pct",
		"match_route/volume_delta",
		"match_route/stop_playing",
		"match_route/fallback_unknown",
		"match_route/music_artist_then_volume",
		"match_route/story_missing_then_volume",
		"match_route/volume_then_music_missing",
		"match_route/fallback_music_stop",
		"match_route/stop_chat",
		"match_route/web_search_current",
		"func_chat/chat_fallback",
		"func_chat/music_direct",
		"func_chat/music_clarify_then_play",
		"func_chat/music_then_volume",
		"func_chat/unknown_then_volume",
	} {
		if !contains(tests, want) {
			t.Fatalf("tests = %v, want %s", tests, want)
		}
	}
	if !strings.Contains(out.String(), "match_route/music_flow") {
		t.Fatalf("config test list output = %q, want match_route/music_flow", out.String())
	}
}

func TestWorkspaceCreateSupportsPersonaConfig(t *testing.T) {
	dir := t.TempDir()
	createWorkspaceFromConfig(t, "boy_14_Tom", dir)
	setExampleEnv(t)
	app, err := openApp(dir)
	if err != nil {
		t.Fatalf("open persona app: %v", err)
	}
	defer app.Close()
	if got := app.Config().Agent.ID; got != "tom" {
		t.Fatalf("persona agent id = %q, want tom", got)
	}
}

func createWorkspaceFromConfig(t *testing.T, source, dir string) {
	t.Helper()
	if err := workspaceCreateCmd([]string{"--config", source, "--workspace", dir}); err != nil {
		t.Fatalf("workspaceCreateCmd %s: %v", source, err)
	}
}

func writeServeTestConfig(t *testing.T, dir string) {
	t.Helper()
	raw := []byte(`{
  "models": {
    "chat": "default",
    "llm": {
      "default": {
        "provider": "mock",
        "model": "mock-default"
      }
    }
  },
  "agent": {
    "id": "serve-test",
    "name": "Serve Test",
    "model": "default"
  }
}`)
	if err := os.WriteFile(filepath.Join(dir, configFileName), raw, 0o644); err != nil {
		t.Fatalf("write serve test config: %v", err)
	}
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

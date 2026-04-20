package api

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/internal/api/oas"
	"github.com/GizClaw/flowcraft/internal/platform"
	"github.com/GizClaw/flowcraft/internal/pluginhost"
	"github.com/GizClaw/flowcraft/plugin"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	ht "github.com/ogen-go/ogen/http"
)

// stubPlugin is a minimal plugin.Plugin used to populate the registry without
// spawning subprocesses. Initialize and Shutdown are no-ops.
type stubPlugin struct {
	info plugin.PluginInfo
}

func (s *stubPlugin) Info() plugin.PluginInfo                              { return s.info }
func (s *stubPlugin) Initialize(_ context.Context, _ map[string]any) error { return nil }
func (s *stubPlugin) Shutdown(_ context.Context) error                     { return nil }

func newPluginHandler(t *testing.T, pluginDir string, pp ...plugin.Plugin) *oapiHandler {
	t.Helper()
	reg := pluginhost.NewRegistry()
	if pluginDir != "" {
		reg.SetExternalManager(pluginhost.NewExternalManager(pluginhost.ExternalManagerConfig{PluginDir: pluginDir}))
	}
	for _, p := range pp {
		if err := reg.Register(p); err != nil {
			t.Fatalf("Register %s: %v", p.Info().ID, err)
		}
	}
	if err := reg.InitializeAll(context.Background()); err != nil {
		t.Fatalf("InitializeAll: %v", err)
	}
	srv := &Server{deps: ServerDeps{Platform: &platform.Platform{PluginReg: reg}}}
	return newOAPIHandler(srv)
}

// ── ListPlugins / GetPlugin ──

func TestListPlugins_NoRegistry(t *testing.T) {
	srv := &Server{deps: ServerDeps{Platform: &platform.Platform{}}}
	h := newOAPIHandler(srv)
	out, err := h.ListPlugins(context.Background(), oas.ListPluginsParams{})
	if err != nil {
		t.Fatalf("ListPlugins: %v", err)
	}
	if out == nil || len(out.Data) != 0 {
		t.Fatalf("expected empty list, got %+v", out)
	}
}

func TestListPlugins_NestedShape(t *testing.T) {
	// Regression: previously the handler used JSON round-trip through a flat
	// PluginDetail, dropping every field except `config`. After the fix the
	// response must carry nested `info` with `id`, `name`, `builtin` etc.
	h := newPluginHandler(t, "",
		&stubPlugin{info: plugin.PluginInfo{
			ID: "tool-x", Name: "Tool X", Version: "1.2.3",
			Type: plugin.TypeTool, Description: "desc", Author: "me",
			Builtin: false,
		}},
		&stubPlugin{info: plugin.PluginInfo{ID: "core", Name: "Core", Builtin: true}},
	)

	out, err := h.ListPlugins(context.Background(), oas.ListPluginsParams{})
	if err != nil {
		t.Fatalf("ListPlugins: %v", err)
	}
	if len(out.Data) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(out.Data))
	}

	byID := map[string]oas.PluginDetail{}
	for _, d := range out.Data {
		byID[d.Info.ID] = d
	}
	if d, ok := byID["tool-x"]; !ok {
		t.Fatal("tool-x missing")
	} else {
		if d.Info.Name != "Tool X" || d.Info.Builtin {
			t.Fatalf("unexpected info: %+v", d.Info)
		}
		if v, _ := d.Info.Version.Get(); v != "1.2.3" {
			t.Fatalf("version not preserved: %q", v)
		}
		if v, _ := d.Info.Type.Get(); v != "tool" {
			t.Fatalf("type not preserved: %q", v)
		}
		if d.Status != oas.PluginDetailStatusActive {
			t.Fatalf("expected active, got %s", d.Status)
		}
	}
}

func TestListPlugins_TypeFilter(t *testing.T) {
	h := newPluginHandler(t, "",
		&stubPlugin{info: plugin.PluginInfo{ID: "a", Name: "A", Type: plugin.TypeTool}},
		&stubPlugin{info: plugin.PluginInfo{ID: "b", Name: "B", Type: plugin.TypeModel}},
	)

	out, err := h.ListPlugins(context.Background(), oas.ListPluginsParams{Type: oas.NewOptString("tool")})
	if err != nil {
		t.Fatalf("ListPlugins: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].Info.ID != "a" {
		t.Fatalf("expected only [a], got %+v", out.Data)
	}
}

func TestGetPlugin_NotFound(t *testing.T) {
	h := newPluginHandler(t, "")
	_, err := h.GetPlugin(context.Background(), oas.GetPluginParams{Name: "missing"})
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestGetPlugin_Found(t *testing.T) {
	h := newPluginHandler(t, "",
		&stubPlugin{info: plugin.PluginInfo{ID: "x", Name: "X", Builtin: true}},
	)
	d, err := h.GetPlugin(context.Background(), oas.GetPluginParams{Name: "x"})
	if err != nil {
		t.Fatalf("GetPlugin: %v", err)
	}
	if d.Info.ID != "x" || !d.Info.Builtin {
		t.Fatalf("unexpected: %+v", d.Info)
	}
}

// ── Enable / Disable / UpdateConfig ──

func TestEnablePlugin_BuiltinRejected(t *testing.T) {
	h := newPluginHandler(t, "",
		&stubPlugin{info: plugin.PluginInfo{ID: "core", Name: "Core", Builtin: true}},
	)
	_, err := h.EnablePlugin(context.Background(), oas.OptPluginConfig{}, oas.EnablePluginParams{Name: "core"})
	if err == nil {
		t.Fatal("expected error toggling built-in plugin")
	}
}

func TestDisablePlugin_External(t *testing.T) {
	h := newPluginHandler(t, "",
		&stubPlugin{info: plugin.PluginInfo{ID: "ext", Name: "Ext", Builtin: false}},
	)
	info, err := h.DisablePlugin(context.Background(), oas.DisablePluginParams{Name: "ext"})
	if err != nil {
		t.Fatalf("DisablePlugin: %v", err)
	}
	if info == nil || info.ID != "ext" {
		t.Fatalf("expected echo of id ext, got %+v", info)
	}
}

func TestEnablePlugin_NotFound(t *testing.T) {
	h := newPluginHandler(t, "")
	_, err := h.EnablePlugin(context.Background(), oas.OptPluginConfig{}, oas.EnablePluginParams{Name: "ghost"})
	if err == nil {
		t.Fatal("expected error for missing plugin")
	}
}

// ── ReloadPlugins ──

func TestReloadPlugins_NoRegistry(t *testing.T) {
	srv := &Server{deps: ServerDeps{Platform: &platform.Platform{}}}
	h := newOAPIHandler(srv)
	out, err := h.ReloadPlugins(context.Background())
	if err != nil {
		t.Fatalf("ReloadPlugins: %v", err)
	}
	if out.Added == nil || out.Removed == nil {
		t.Fatalf("expected non-nil slices, got %+v", out)
	}
}

func TestReloadPlugins_RemovesMissingExternal(t *testing.T) {
	// Regression: ReloadPlugins used to return empty-string slices sized by
	// counts (e.g. {"added":["",""]}). After the fix it must return real IDs.
	dir := t.TempDir()
	h := newPluginHandler(t, dir,
		&stubPlugin{info: plugin.PluginInfo{ID: "ghost", Name: "Ghost", Builtin: false}},
	)
	out, err := h.ReloadPlugins(context.Background())
	if err != nil {
		t.Fatalf("ReloadPlugins: %v", err)
	}
	if len(out.Removed) != 1 || out.Removed[0] != "ghost" {
		t.Fatalf("expected [ghost] removed, got %v", out.Removed)
	}
	if len(out.Added) != 0 {
		t.Fatalf("expected 0 added, got %v", out.Added)
	}
}

// ── UploadPlugin ──

func TestUploadPlugin_RequiresFilename(t *testing.T) {
	h := newPluginHandler(t, t.TempDir())
	_, err := h.UploadPlugin(context.Background(), &oas.UploadPluginReq{
		File: ht.MultipartFile{Name: "  ", File: strings.NewReader("payload")},
	})
	if err == nil {
		t.Fatal("expected validation error for blank filename")
	}
}

func TestUploadPlugin_RejectsTraversal(t *testing.T) {
	h := newPluginHandler(t, t.TempDir())
	for _, name := range []string{"../evil", "sub/bin", `c:\nope`, ".hidden"} {
		_, err := h.UploadPlugin(context.Background(), &oas.UploadPluginReq{
			File: ht.MultipartFile{Name: name, File: strings.NewReader("x")},
		})
		if err == nil {
			t.Fatalf("expected rejection for %q", name)
		}
	}
}

func TestUploadPlugin_NoRegistry(t *testing.T) {
	srv := &Server{deps: ServerDeps{Platform: &platform.Platform{}}}
	h := newOAPIHandler(srv)
	_, err := h.UploadPlugin(context.Background(), &oas.UploadPluginReq{
		File: ht.MultipartFile{Name: "x", File: strings.NewReader("x")},
	})
	if err == nil {
		t.Fatal("expected error when registry is not configured")
	}
}

func TestUploadPlugin_WritesBinaryAndReports(t *testing.T) {
	dir := t.TempDir()
	h := newPluginHandler(t, dir)
	body := "hello-binary"
	out, err := h.UploadPlugin(context.Background(), &oas.UploadPluginReq{
		File: ht.MultipartFile{
			Name: "echo",
			File: io.NopCloser(strings.NewReader(body)),
			Size: int64(len(body)),
		},
	})
	if err != nil {
		t.Fatalf("UploadPlugin: %v", err)
	}

	// File must land on disk with executable bits.
	written, statErr := os.Stat(filepath.Join(dir, "echo"))
	if statErr != nil {
		t.Fatalf("binary missing: %v", statErr)
	}
	if written.Mode()&0o111 == 0 {
		t.Fatal("uploaded binary is not executable")
	}

	// Reported size matches what we wrote.
	if v, _ := out.Size.Get(); v != len(body) {
		t.Fatalf("size mismatch: %d", v)
	}
	if v, _ := out.Name.Get(); v != "echo" {
		t.Fatalf("name mismatch: %q", v)
	}
	if out.Added == nil || out.Removed == nil {
		t.Fatalf("added/removed should be non-nil slices, got %+v", out)
	}
}

// ── DeletePlugin ──

func TestDeletePlugin_NotFound(t *testing.T) {
	h := newPluginHandler(t, t.TempDir())
	err := h.DeletePlugin(context.Background(), oas.DeletePluginParams{Name: "ghost"})
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestDeletePlugin_BuiltinRejected(t *testing.T) {
	h := newPluginHandler(t, t.TempDir(),
		&stubPlugin{info: plugin.PluginInfo{ID: "core", Name: "Core", Builtin: true}},
	)
	err := h.DeletePlugin(context.Background(), oas.DeletePluginParams{Name: "core"})
	if err == nil {
		t.Fatal("expected error removing built-in plugin")
	}
}

func TestDeletePlugin_RemovesExternalBinary(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "ext")
	if err := os.WriteFile(binPath, []byte("noop"), 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	h := newPluginHandler(t, dir,
		// Use a plain stubPlugin so no subprocess is involved. The handler will
		// look up the binary path by joining PluginDir/<id>.
		&stubPlugin{info: plugin.PluginInfo{ID: "ext", Name: "Ext", Builtin: false}},
	)

	if err := h.DeletePlugin(context.Background(), oas.DeletePluginParams{Name: "ext"}); err != nil {
		t.Fatalf("DeletePlugin: %v", err)
	}
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Fatalf("binary should be removed, stat err=%v", err)
	}
}

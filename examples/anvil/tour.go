package main

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/craft"
)

const (
	// pluginTool and hostTool are the hello plugin's two MCP tools.
	// craft prefixes plugin tools with the plugin id, so the server's
	// own names are greet and ping_host.
	pluginTool = "hello__greet"
	hostTool   = "hello__ping_host"

	// alphaKey is an application-defined runtime key; its meaning (a
	// workspace, a profile, a tenant) belongs to the application.
	alphaKey = craft.RuntimeKey("alpha")
)

// tour walks the craft lifecycle once and narrates it. Every step is
// something a shell does for real: scan, start, open runtimes by key,
// call tools, reload, hot-plug, shut down.
func (a *app) tour(ctx context.Context) error {
	// The safety net for the failure paths; the shutdown step closes
	// explicitly and Close is idempotent.
	defer func() { _ = a.craft.Close() }()

	// The tour writes scratch files under the data directory; make sure
	// a previous run cannot leak into this one.
	removed, err := a.resetScratch()
	if err != nil {
		return err
	}

	a.print.printf("anvil: a minimal craft host (craft module %s, definition %s)\n",
		craft.Version, a.def.Craft.ID)

	a.print.step("definition")
	a.print.line("craft %s %s (%s)",
		a.def.Craft.ID, a.def.Craft.Version, a.def.Craft.Name)
	a.print.line("file: %s", a.definition)
	a.print.line("deploy resources: %s", strings.Join(a.resourceNames(), ", "))
	a.print.line("data directory: %s (%d scratch files removed)",
		a.dataDir, removed)

	// craft.yaml declares the roots; the application scans them.
	a.print.step("plugins")
	summaries, err := a.plugins.List()
	if err != nil {
		return err
	}
	for _, summary := range summaries {
		a.print.line("%s %s  enabled=%v  permissions=%s",
			summary.ID, summary.Version, summary.Enabled,
			strings.Join(summary.Permissions, ", "))
	}
	for _, root := range a.plugins.SkillRoots() {
		a.print.line("skill root: %s", root)
	}

	a.print.step("start")
	if err := a.craft.Start(ctx); err != nil {
		return err
	}
	a.print.line("plugin host revision %d", a.plugins.Revision())
	a.print.line("waiting for %s (the plugin's MCP server is a Go program; the first run compiles it)",
		pluginTool)
	if err := a.waitForPluginTool(ctx, pluginTool, a.opts.pluginTimeout); err != nil {
		return err
	}
	a.print.line("plugin tools published: %s", strings.Join(a.pluginToolNames(), ", "))

	// A runtime opens per key, with its own values. ${craft:notes} in
	// the definition resolves from them.
	a.print.step(`runtime "default"`)
	defaultRuntime, err := a.craft.OpenRuntime(ctx, craft.DefaultKey, craft.RuntimeOptions{
		Values: map[string]string{
			"notes": filepath.Join(a.dataDir, "notes-default.txt"),
		},
	})
	if err != nil {
		return err
	}
	if err := a.showTools(defaultRuntime); err != nil {
		return err
	}
	description, err := a.describe(defaultRuntime, "notes_add")
	if err != nil {
		return err
	}
	a.print.line("notes_add  %q", description)
	if _, err := a.call(ctx, defaultRuntime, "notes_add",
		map[string]any{"text": "buy milk"}); err != nil {
		return err
	}
	if _, err := a.call(ctx, defaultRuntime, "notes_list",
		map[string]any{}); err != nil {
		return err
	}
	if _, err := a.call(ctx, defaultRuntime, pluginTool,
		map[string]any{"name": "anvil"}); err != nil {
		return err
	}
	if _, err := a.call(ctx, defaultRuntime, hostTool,
		map[string]any{}); err != nil {
		return err
	}

	// The second runtime proves the keyed set: same definition, its own
	// tool catalog, its own notes file. Its file comes from an
	// application-written per-runtime layer instead of a value.
	a.print.step(`runtime "alpha"`)
	alphaLayer, err := a.writeAlphaLayer(filepath.Join(a.dataDir, "notes-alpha.txt"))
	if err != nil {
		return err
	}
	a.print.line("layer %s: notes.path -> notes-alpha.txt", alphaLayer)
	alphaRuntime, err := a.craft.OpenRuntime(ctx, alphaKey, craft.RuntimeOptions{
		Layers: []deploy.Layer{{
			Name:    "alpha",
			Source:  resource.Source{File: filepath.Base(alphaLayer)},
			BaseDir: a.dataDir,
		}},
	})
	if err != nil {
		return err
	}
	if err := a.showTools(alphaRuntime); err != nil {
		return err
	}
	description, err = a.describe(alphaRuntime, "notes_add")
	if err != nil {
		return err
	}
	a.print.line("notes_add  %q", description)
	if _, err := a.call(ctx, alphaRuntime, "notes_list",
		map[string]any{}); err != nil {
		return err
	}
	if _, err := a.call(ctx, alphaRuntime, "notes_add",
		map[string]any{"text": "call the builder"}); err != nil {
		return err
	}

	// Reloading re-reads every layer, so rewriting the alpha layer and
	// reloading swaps the generation that serves it.
	a.print.step(`reload "alpha"`)
	round2 := filepath.Join(a.dataDir, "notes-alpha-round2.txt")
	if _, err := a.writeAlphaLayer(round2); err != nil {
		return err
	}
	a.print.line("rewrote the layer: notes.path -> %s", filepath.Base(round2))
	if err := a.craft.ReloadRuntime(ctx, alphaKey, craft.ReasonManual); err != nil {
		return err
	}
	if _, err := a.call(ctx, alphaRuntime, "notes_list",
		map[string]any{}); err != nil {
		return err
	}

	// A plugin change touches the deployment document and the shared
	// tool set, never the resource registry: that is what makes
	// hot-plugging safe.
	a.print.step("hot plug: disable and enable the hello plugin")
	mark := a.events.mark()
	if err := a.plugins.SetEnabled(ctx, "hello", false); err != nil {
		return err
	}
	if err := a.events.wait(ctx, mark, craft.SubjectReloadCompleted, a.opts.reloadTimeout); err != nil {
		return err
	}
	if err := a.waitForCatalog(ctx, defaultRuntime, pluginTool, false, a.opts.reloadTimeout); err != nil {
		return err
	}
	if err := a.showTools(defaultRuntime); err != nil {
		return err
	}
	a.print.line("disabled: the plugin's tools are withdrawn from every open runtime")

	mark = a.events.mark()
	if err := a.plugins.SetEnabled(ctx, "hello", true); err != nil {
		return err
	}
	if err := a.waitForPluginTool(ctx, pluginTool, a.opts.pluginTimeout); err != nil {
		return err
	}
	if err := a.events.wait(ctx, mark, craft.SubjectReloadCompleted, a.opts.reloadTimeout); err != nil {
		return err
	}
	if err := a.waitForCatalog(ctx, defaultRuntime, pluginTool, true, a.opts.reloadTimeout); err != nil {
		return err
	}
	if err := a.showTools(defaultRuntime); err != nil {
		return err
	}
	if _, err := a.call(ctx, defaultRuntime, pluginTool,
		map[string]any{"name": "the restarted plugin"}); err != nil {
		return err
	}

	a.print.step("shutdown")
	if err := a.craft.CloseRuntime(ctx, alphaKey); err != nil {
		return err
	}
	if err := a.craft.Close(); err != nil {
		return err
	}
	a.print.line("craft closed")

	a.print.step("read next")
	a.print.line("docs/guides/craft.md — the definition schema, lifecycle and errors")
	a.print.line("skills/flowcraft-config — craft.yaml and plugin.json reference cards")
	a.print.line("craft/hostmcp — the v1 host primitive set, the plugin's other half")
	return nil
}

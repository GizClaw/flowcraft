package bootstrap

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/sandbox"
	"github.com/GizClaw/flowcraft/internal/skill"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/extract"
	"github.com/GizClaw/flowcraft/skills"

	otellog "go.opentelemetry.io/otel/log"
)

// wireSkill creates the skill store, starts the file watcher, registers
// SkillTool and the extract tool. The returned cleanup stops the watcher.
func wireSkill(ctx context.Context, ws workspace.Workspace, cfg *config.Config, sandboxMgr *sandbox.Manager, toolReg *tool.Registry) (*skill.SkillStore, func(), error) {
	skillStore := skill.NewSkillStore(ws, "skills")
	skillStore.SetBuiltinFS(skills.BuiltinFS())
	skillStore.SetGlobalConfig(cfg.Skills)
	if err := skillStore.BuildIndex(ctx); err != nil {
		telemetry.Warn(ctx, "skill: initial index build failed", otellog.String("error", err.Error()))
	}

	var cleanups []func()

	skillWatcher, swErr := skillStore.StartWatching(ctx)
	if swErr != nil {
		telemetry.Warn(ctx, "skill: fsnotify watcher disabled", otellog.String("error", swErr.Error()))
	}
	if skillWatcher != nil {
		cleanups = append(cleanups, func() { skillWatcher.Stop() })
	}

	skillExecutor := skill.NewSkillExecutor(skillStore, sandboxMgr)
	toolReg.Register(&skill.SkillTool{Store: skillStore, Executor: skillExecutor})
	extract.Register(toolReg, extract.New())

	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	return skillStore, cleanup, nil
}

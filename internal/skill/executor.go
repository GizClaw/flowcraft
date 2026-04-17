package skill

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/sandbox"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// SkillExecutor runs skills inside a sandbox.
type SkillExecutor struct {
	store   *SkillStore
	manager *sandbox.Manager
}

// NewSkillExecutor creates an executor backed by the given store and sandbox manager.
func NewSkillExecutor(store *SkillStore, manager *sandbox.Manager) *SkillExecutor {
	return &SkillExecutor{store: store, manager: manager}
}

// Execute runs a skill by name with the given arguments in a sandbox.
func (e *SkillExecutor) Execute(ctx context.Context, skillName, args string) (string, error) {
	if !e.store.IsEnabled(skillName) {
		return "", fmt.Errorf("skill: %q is disabled", skillName)
	}

	meta, ok := e.store.Get(skillName)
	if !ok {
		return "", fmt.Errorf("skill: %q not found", skillName)
	}

	sb, release, err := e.acquireSandbox(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	if err := e.installDeps(ctx, sb, meta); err != nil {
		return "", fmt.Errorf("skill: dependency installation failed for %s: %w", skillName, err)
	}

	cmd, cmdArgs := buildCommand(ctx, meta, args)

	execOpts := sandbox.ExecOptions{
		WorkDir: meta.Dir,
		Timeout: 5 * time.Minute,
	}
	if env := e.store.ResolveEnv(skillName); len(env) > 0 {
		execOpts.Env = env
	}

	result, err := sb.Exec(ctx, cmd, cmdArgs, execOpts)
	if err != nil {
		return "", fmt.Errorf("skill: exec %s: %w", skillName, err)
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("skill: %s exited with code %d: %s", skillName, result.ExitCode, result.Stderr)
	}

	return result.Stdout, nil
}

func (e *SkillExecutor) acquireSandbox(ctx context.Context) (sandbox.Sandbox, func(), error) {
	if handle, ok := model.SandboxHandleFrom(ctx).(*sandbox.SandboxHandle); ok && handle != nil {
		sb, release, err := handle.Acquire(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("skill: acquire sandbox handle: %w", err)
		}
		return sb, release, nil
	}

	if e.manager == nil {
		return nil, nil, fmt.Errorf("skill: sandbox manager not available")
	}

	runtimeID := model.RuntimeIDFrom(ctx)
	if runtimeID == "" {
		return nil, nil, fmt.Errorf("skill: no runtime ID in context")
	}

	sb, err := e.manager.Acquire(ctx, runtimeID, sandbox.AcquireOptions{Mode: sandbox.ModePersistent})
	if err != nil {
		return nil, nil, fmt.Errorf("skill: acquire sandbox: %w", err)
	}
	release := func() {
		if err := e.manager.Release(runtimeID); err != nil {
			telemetry.Warn(ctx, "skill: release sandbox", otellog.String("error", err.Error()))
		}
	}
	return sb, release, nil
}

func (e *SkillExecutor) installDeps(ctx context.Context, sb sandbox.Sandbox, meta *SkillMeta) error {
	// Check for requirements.txt (Python)
	if data, err := sb.ReadFile(ctx, meta.Dir+"/requirements.txt"); err == nil && len(data) > 0 {
		result, err := sb.Exec(ctx, "pip", []string{"install", "-r", "requirements.txt"}, sandbox.ExecOptions{
			WorkDir: meta.Dir,
			Timeout: 2 * time.Minute,
		})
		if err != nil {
			return fmt.Errorf("pip install: %w", err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("pip install exited with code %d: %s", result.ExitCode, result.Stderr)
		}
	}

	// Check for package.json (Node.js)
	if data, err := sb.ReadFile(ctx, meta.Dir+"/package.json"); err == nil && len(data) > 0 {
		result, err := sb.Exec(ctx, "npm", []string{"install"}, sandbox.ExecOptions{
			WorkDir: meta.Dir,
			Timeout: 2 * time.Minute,
		})
		if err != nil {
			return fmt.Errorf("npm install: %w", err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("npm install exited with code %d: %s", result.ExitCode, result.Stderr)
		}
	}

	return nil
}

func buildCommand(ctx context.Context, meta *SkillMeta, args string) (string, []string) {
	entry := meta.Entry
	switch {
	case hasSuffix(entry, ".py"):
		cmdArgs := []string{entry}
		if args != "" {
			cmdArgs = append(cmdArgs, args)
		}
		return "python3", cmdArgs
	case hasSuffix(entry, ".js"):
		cmdArgs := []string{entry}
		if args != "" {
			cmdArgs = append(cmdArgs, args)
		}
		return "node", cmdArgs
	case hasSuffix(entry, ".ts"):
		// Use tsx for TypeScript execution (supports ESM natively)
		cmdArgs := []string{entry}
		if args != "" {
			cmdArgs = append(cmdArgs, args)
		}
		return "npx", append([]string{"tsx"}, cmdArgs...)
	case hasSuffix(entry, ".go"):
		cmdArgs := []string{entry}
		if args != "" {
			cmdArgs = append(cmdArgs, args)
		}
		return "go", append([]string{"run"}, cmdArgs...)
	case hasSuffix(entry, ".sh"):
		cmdArgs := []string{entry}
		if args != "" {
			cmdArgs = append(cmdArgs, args)
		}
		return "bash", cmdArgs
	default:
		// Warn about unknown extensions - assuming executable file
		telemetry.Warn(ctx, "skill: unknown entry file extension, treating as executable",
			otellog.String("skill", meta.Name),
			otellog.String("entry", entry))
		cmdArgs := []string{}
		if args != "" {
			cmdArgs = append(cmdArgs, args)
		}
		return entry, cmdArgs
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

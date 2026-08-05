package commands

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
	"github.com/GizClaw/flowcraft/examples/forge/internal/scenario"
)

type testFile struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Raid        string   `yaml:"raid"`
	Turns       []string `yaml:"turns"`
}

type testMetrics struct {
	Workspace  string
	StartedAt  time.Time
	FinishedAt time.Time
	Elapsed    time.Duration
	Timeout    time.Duration
	Turns      []testTurnMetric
}

type testTurnMetric struct {
	Turn        int
	Input       string
	StartedAt   time.Time
	FinishedAt  time.Time
	Elapsed     time.Duration
	TokenEvents int
	ToolCalls   int
	OutputChars int
	Error       string
}

func testCmd(args []string) error {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	testSource := flags.String("test", "", "test path or embedded test name")
	timeout := flags.Duration("timeout", 2*time.Minute, "maximum duration per test turn")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *testSource == "" {
		return fmt.Errorf("test requires -test\n\n%s", usage())
	}
	if *timeout <= 0 {
		return fmt.Errorf("test requires --timeout > 0")
	}
	_, raw, err := scenario.ReadTestSource(*testSource)
	if err != nil {
		return err
	}
	var test testFile
	if err := yaml.Unmarshal(raw, &test); err != nil {
		return fmt.Errorf("test %q: %w", *testSource, err)
	}
	raid := strings.TrimSpace(test.Raid)
	if raid == "" {
		return fmt.Errorf("test %q requires raid", *testSource)
	}
	if len(test.Turns) == 0 {
		return fmt.Errorf("test %q requires at least one turn", *testSource)
	}
	outputDir := testRunDir(raid, time.Now())
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create test output directory: %w", err)
	}
	workspacePath := filepath.Join(outputDir, "workspace")
	raidRef, err := scenario.Resolve("raids", raid)
	if err != nil {
		return fmt.Errorf("resolve raid scenario: %w", err)
	}
	if err := scenario.Copy(raidRef, workspacePath); err != nil {
		return fmt.Errorf("create test workspace: %w", err)
	}
	metrics, err := runTestTurns(workspacePath, filepath.Join(outputDir, "chat_log.txt"), test.Turns, *timeout)
	if writeErr := writeTestStats(filepath.Join(outputDir, "stats.txt"), metrics); writeErr != nil && err == nil {
		err = writeErr
	}
	if err != nil {
		return err
	}
	fmt.Printf("wrote test %s\n", outputDir)
	return nil
}

func runTestTurns(workspacePath, logPath string, inputs []string, timeout time.Duration) (testMetrics, error) {
	metrics := testMetrics{
		Workspace: workspacePath,
		StartedAt: time.Now(),
		Timeout:   timeout,
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout*time.Duration(len(inputs))+30*time.Second)
	defer cancel()
	a, err := app.Open(ctx, workspacePath)
	if err != nil {
		metrics.FinishedAt = time.Now()
		metrics.Elapsed = metrics.FinishedAt.Sub(metrics.StartedAt)
		return metrics, fmt.Errorf("open test workspace: %w", err)
	}
	defer func() { _ = a.Close() }()

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return metrics, fmt.Errorf("open test chat log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	fmt.Fprintf(logFile, "# workspace: %s\n\n", workspacePath)

	for i, input := range inputs {
		turnMetric, text, err := runOneTestTurn(ctx, a, i+1, input, timeout)
		metrics.Turns = append(metrics.Turns, turnMetric)
		if err != nil {
			return finishTestMetrics(metrics), err
		}
		var turn strings.Builder
		fmt.Fprintf(&turn, "=== Turn %d ===\n", i+1)
		fmt.Fprintf(&turn, "user: %s\n", input)
		fmt.Fprintf(&turn, "assistant: %s\n\n", text)
		if _, err := logFile.WriteString(turn.String()); err != nil {
			return finishTestMetrics(metrics), err
		}
	}
	if err := logFile.Sync(); err != nil {
		return finishTestMetrics(metrics), err
	}
	return finishTestMetrics(metrics), nil
}

func runOneTestTurn(
	ctx context.Context,
	a *app.App,
	turn int,
	input string,
	timeout time.Duration,
) (testTurnMetric, string, error) {
	metric := testTurnMetric{Turn: turn, Input: input, StartedAt: time.Now()}
	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	toolsBefore := a.ToolCalls()
	collector := &textCollectorSink{}
	result, err := a.RunTurn(turnCtx, input, collector.spec())
	metric.FinishedAt = time.Now()
	metric.Elapsed = metric.FinishedAt.Sub(metric.StartedAt)
	metric.TokenEvents = collector.tokens
	metric.ToolCalls = int(a.ToolCalls() - toolsBefore)
	metric.OutputChars = collector.builder.Len()
	if err != nil {
		metric.Error = err.Error()
		return metric, "", err
	}
	if metric.OutputChars == 0 && result != nil && len(result.Messages) > 0 {
		metric.OutputChars = len(result.Messages[len(result.Messages)-1].Content.Text())
	}
	return metric, collector.builder.String(), nil
}

func finishTestMetrics(metrics testMetrics) testMetrics {
	metrics.FinishedAt = time.Now()
	metrics.Elapsed = metrics.FinishedAt.Sub(metrics.StartedAt)
	return metrics
}

func writeTestStats(outputPath string, metrics testMetrics) error {
	var out strings.Builder
	fmt.Fprintln(&out, "--- forge test run ---")
	fmt.Fprintf(&out, "workspace: %s\n", metrics.Workspace)
	fmt.Fprintf(&out, "started_at: %s\n", metrics.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&out, "finished_at: %s\n", metrics.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&out, "elapsed: %s\n", metrics.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(&out, "turns_completed: %d\n", len(metrics.Turns))
	fmt.Fprintf(&out, "timeout: %s\n", metrics.Timeout)
	for _, turn := range metrics.Turns {
		fmt.Fprintf(&out, "\n--- turn %d ---\n", turn.Turn)
		fmt.Fprintf(&out, "input: %s\n", turn.Input)
		fmt.Fprintf(&out, "started_at: %s\n", turn.StartedAt.Format(time.RFC3339))
		fmt.Fprintf(&out, "finished_at: %s\n", turn.FinishedAt.Format(time.RFC3339))
		fmt.Fprintf(&out, "elapsed: %s\n", turn.Elapsed.Round(time.Millisecond))
		fmt.Fprintf(&out, "token_events: %d\n", turn.TokenEvents)
		fmt.Fprintf(&out, "tool_calls: %d\n", turn.ToolCalls)
		fmt.Fprintf(&out, "output_chars: %d\n", turn.OutputChars)
		if turn.Error != "" {
			fmt.Fprintf(&out, "error: %s\n", turn.Error)
		}
	}
	return os.WriteFile(outputPath, []byte(out.String()), 0o644)
}

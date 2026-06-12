package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdkx/claw"
)

type testMetrics struct {
	Workspace  string
	StartedAt  time.Time
	FinishedAt time.Time
	Elapsed    time.Duration
	Timeout    time.Duration
	Turns      []testTurnMetric
}

type testTurnMetric struct {
	Turn              int
	Input             string
	StartedAt         time.Time
	FirstTokenAt      time.Time
	FinishedAt        time.Time
	Elapsed           time.Duration
	FirstTokenLatency time.Duration
	TokenEvents       int
	ToolCalls         int
	ToolResults       int
	OutputChars       int
	Error             string
}

func runTestTurns(workspacePath, logPath string, inputs []string, timeout time.Duration) (testMetrics, error) {
	metrics := testMetrics{
		Workspace: workspacePath,
		StartedAt: time.Now(),
		Timeout:   timeout,
	}
	app, err := openApp(workspacePath)
	if err != nil {
		metrics.FinishedAt = time.Now()
		metrics.Elapsed = metrics.FinishedAt.Sub(metrics.StartedAt)
		return metrics, fmt.Errorf("open test workspace: %w", err)
	}
	defer func() { _ = app.Close() }()
	attachSimulatedToolHandler(app)

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return finishTestMetrics(metrics), fmt.Errorf("create test output directory: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return finishTestMetrics(metrics), fmt.Errorf("open test chat log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	startTurn, err := nextTestTurn(logPath)
	if err != nil {
		return finishTestMetrics(metrics), err
	}
	if startTurn == 1 {
		fmt.Fprintf(logFile, "# workspace: %s\n\n", workspacePath)
	}
	for i, input := range inputs {
		text, eventLog, turnMetric, err := runOneTestTurn(app, startTurn+i, input, timeout)
		metrics.Turns = append(metrics.Turns, turnMetric)
		if err != nil {
			return finishTestMetrics(metrics), err
		}
		var turn strings.Builder
		appendAutoTurn(&turn, startTurn+i,
			autoSpeech{Actor: "user", Text: input},
			autoSpeech{Actor: "assistant", Text: text, EventLog: eventLog.String()},
		)
		if _, err := logFile.WriteString(turn.String()); err != nil {
			return finishTestMetrics(metrics), fmt.Errorf("write test chat log: %w", err)
		}
	}
	if err := logFile.Sync(); err != nil {
		return finishTestMetrics(metrics), err
	}
	return finishTestMetrics(metrics), nil
}

func finishTestMetrics(metrics testMetrics) testMetrics {
	metrics.FinishedAt = time.Now()
	metrics.Elapsed = metrics.FinishedAt.Sub(metrics.StartedAt)
	return metrics
}

func runOneTestTurn(app *claw.Claw, turn int, input string, timeout time.Duration) (string, *autoEventLog, testTurnMetric, error) {
	metric := testTurnMetric{
		Turn:      turn,
		Input:     input,
		StartedAt: time.Now(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := app.RoundTrip(claw.Request{
		Context: ctx,
		Text:    input,
	})
	if err != nil {
		metric.FinishedAt = time.Now()
		metric.Elapsed = metric.FinishedAt.Sub(metric.StartedAt)
		metric.Error = err.Error()
		return "", nil, metric, fmt.Errorf("run test turn: %w", err)
	}
	eventLog := newAutoEventLog()
	var text strings.Builder
	for {
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			metric.FinishedAt = time.Now()
			metric.Elapsed = metric.FinishedAt.Sub(metric.StartedAt)
			metric.OutputChars = len(text.String())
			return text.String(), eventLog, metric, nil
		}
		if err != nil {
			metric.FinishedAt = time.Now()
			metric.Elapsed = metric.FinishedAt.Sub(metric.StartedAt)
			metric.Error = err.Error()
			return "", nil, metric, err
		}
		if ev.Type == claw.EventToken {
			if metric.FirstTokenAt.IsZero() {
				metric.FirstTokenAt = time.Now()
				metric.FirstTokenLatency = metric.FirstTokenAt.Sub(metric.StartedAt)
			}
			metric.TokenEvents++
			text.WriteString(ev.Content)
		}
		if ev.Type == claw.EventToolCall {
			metric.ToolCalls++
		}
		if ev.Type == claw.EventToolResult {
			metric.ToolResults++
		}
		eventLog.Record(ev)
	}
}

func nextTestTurn(logPath string) (int, error) {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 1, nil
		}
		return 0, fmt.Errorf("read test chat log: %w", err)
	}
	return strings.Count(string(raw), "=== Turn ") + 1, nil
}

func writeTestStats(outputPath string, metrics testMetrics) error {
	var out strings.Builder
	fmt.Fprintln(&out, "--- runtime inspect ---")
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
		if turn.FirstTokenAt.IsZero() {
			fmt.Fprintln(&out, "first_token_at: none")
			fmt.Fprintln(&out, "first_token_latency: none")
		} else {
			fmt.Fprintf(&out, "first_token_at: %s\n", turn.FirstTokenAt.Format(time.RFC3339Nano))
			fmt.Fprintf(&out, "first_token_latency: %s\n", turn.FirstTokenLatency.Round(time.Millisecond))
		}
		fmt.Fprintf(&out, "token_events: %d\n", turn.TokenEvents)
		fmt.Fprintf(&out, "tool_calls: %d\n", turn.ToolCalls)
		fmt.Fprintf(&out, "tool_results: %d\n", turn.ToolResults)
		fmt.Fprintf(&out, "output_chars: %d\n", turn.OutputChars)
		if turn.Error != "" {
			fmt.Fprintf(&out, "error: %s\n", turn.Error)
		}
	}
	if err := os.WriteFile(outputPath, []byte(out.String()), 0o644); err != nil {
		return fmt.Errorf("write test stats: %w", err)
	}
	return nil
}

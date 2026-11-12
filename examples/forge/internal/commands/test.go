package commands

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
	"github.com/GizClaw/flowcraft/examples/forge/internal/chatfmt"
	"github.com/GizClaw/flowcraft/examples/forge/internal/scenario"

	"sigs.k8s.io/yaml"
)

// testFile is one scripted scenario test.
type testFile struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Raid        string   `json:"raid"`
	Turns       []string `json:"turns"`
}

// turnObserver folds a turn's events into what the test reports: chat blocks,
// token accounting and timing.
type turnObserver struct {
	started   time.Time
	first     time.Time
	blocks    *chatfmt.Collector
	usage     app.Usage
	toolCalls int
}

func newTurnObserver() *turnObserver {
	return &turnObserver{started: time.Now(), blocks: chatfmt.NewCollector()}
}

// observeSeat folds one seat's event in, under its label and the phase it acted in.
func (o *turnObserver) observeSeat(a *app.App, seat app.SeatEvent) {
	o.blocks.SetSpeaker(seat.Label)
	o.blocks.SetPhase(seat.Phase, a.Private(seat.Phase))
	o.blocks.Observe(seat.Event)
}

// drainSeats takes what the seats said during the turn. Delegation runs inside the
// turn, so everything a seat produced is already buffered when the turn returns;
// nothing here waits.
func (o *turnObserver) drainSeats(a *app.App) {
	for {
		select {
		case seat, ok := <-a.Events():
			if !ok {
				return
			}
			o.observeSeat(a, seat)
		default:
			return
		}
	}
}

func (o *turnObserver) observe(event agent.Event) {
	switch typed := event.(type) {
	case agent.Text:
		if o.first.IsZero() && strings.TrimSpace(typed.Text) != "" {
			o.first = time.Now()
		}
	case agent.ToolCallStart:
		o.toolCalls++
	case agent.UsageEvent:
		o.usage.Add(typed.Usage)
	}
	o.blocks.Observe(event)
}

// firstTokenLatency is how long the turn ran before its first text increment.
func (o *turnObserver) firstTokenLatency() time.Duration {
	if o.first.IsZero() {
		return 0
	}
	return o.first.Sub(o.started)
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
	FirstToken  time.Duration
	TokenEvents int
	ToolCalls   int
	OutputChars int
	Usage       app.Usage
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
	test, err := parseTestFile(raw)
	if err != nil {
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

// parseTestFile decodes a scenario test document. YAML and JSON are both
// accepted: the authoring form is YAML, the decoder is the same one the
// deployment documents use.
func parseTestFile(data []byte) (testFile, error) {
	var test testFile
	if err := yaml.Unmarshal(data, &test); err != nil {
		return testFile{}, err
	}
	return test, nil
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
	_, _ = fmt.Fprintf(logFile, "# workspace: %s\n\n", workspacePath)

	for i, input := range inputs {
		metric, rendered, err := runOneTestTurn(ctx, a, i+1, input, timeout)
		metrics.Turns = append(metrics.Turns, metric)
		if err != nil {
			return finishTestMetrics(metrics), err
		}
		var turn strings.Builder
		fmt.Fprintf(&turn, "=== Turn %d ===\n", i+1)
		fmt.Fprintf(&turn, "user: %s\n", input)
		fmt.Fprintf(&turn, "assistant:\n%s\n\n", rendered)
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
	metric := testTurnMetric{Turn: turn, Input: input}
	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	observer := newTurnObserver()
	// A node the deployment calls silent reports to the graph, not to the table.
	observer.blocks.SetSilent(a.Silent)
	result, err := a.RunTurn(turnCtx, a.Info().Agent, input, observer.observe)
	observer.drainSeats(a)
	metric.StartedAt = observer.started
	metric.FinishedAt = time.Now()
	metric.Elapsed = metric.FinishedAt.Sub(metric.StartedAt)
	metric.FirstToken = observer.firstTokenLatency()
	metric.TokenEvents = observer.blocks.Tokens()
	metric.ToolCalls = observer.toolCalls
	metric.OutputChars = len(observer.blocks.Text())
	metric.Usage = observer.usage
	if failure := app.Failure(result, err); failure != nil {
		metric.Error = failure.Error()
		return metric, "", failure
	}
	rendered := chatfmt.Render(observer.blocks.Blocks())
	if text := chatfmt.ResultText(result); rendered == "" && text != "" {
		rendered = text
	}
	return metric, rendered, nil
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
		fmt.Fprintf(&out, "elapsed: %s\n", turn.Elapsed.Round(time.Millisecond))
		fmt.Fprintf(&out, "first_token_latency: %s\n", turn.FirstToken.Round(time.Millisecond))
		fmt.Fprintf(&out, "token_events: %d\n", turn.TokenEvents)
		fmt.Fprintf(&out, "tool_calls: %d\n", turn.ToolCalls)
		fmt.Fprintf(&out, "output_chars: %d\n", turn.OutputChars)
		fmt.Fprintf(&out, "tokens_in: %d\ntokens_out: %d\ncalls: %d\n",
			turn.Usage.InputTokens, turn.Usage.OutputTokens, turn.Usage.Calls)
		if turn.Error != "" {
			fmt.Fprintf(&out, "error: %s\n", turn.Error)
		}
	}
	return os.WriteFile(outputPath, []byte(out.String()), 0o644)
}

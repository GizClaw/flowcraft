package commands

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
	"github.com/GizClaw/flowcraft/examples/forge/internal/chatfmt"
	"github.com/GizClaw/flowcraft/examples/forge/internal/scenario"
)

// personaOpening is handed to the persona to start the dialogue: the persona is
// a second deployed agent that generates the user-side lines.
const personaOpening = "你正在进入一个互动对话副本，对话会像语音聊天一样一来一回。请以你的角色用一句自然中文开场，只输出会被直接朗读出来的话，不要写占位符、舞台说明或内心想法。"

func testAutoCmd(args []string) error {
	flags := flag.NewFlagSet("test-auto", flag.ContinueOnError)
	raidSource := flags.String("raid", "", "raid scenario name or path")
	personaSource := flags.String("persona", "", "persona scenario name or path")
	timeout := flags.Duration("timeout", 5*time.Minute, "maximum duration per simulated turn")
	turns := flags.Int("turns", 3, "number of dialogue rounds (persona + raid each)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *raidSource == "" || *personaSource == "" {
		return fmt.Errorf("test-auto requires --raid and --persona\n\n%s", usage())
	}
	if *turns <= 0 {
		return fmt.Errorf("test-auto requires --turns > 0")
	}
	raidRef, err := scenario.Resolve("raids", *raidSource)
	if err != nil {
		return err
	}
	personaRef, err := scenario.Resolve("personas", *personaSource)
	if err != nil {
		return err
	}

	outputDir := testRunDir(*raidSource+"-"+*personaSource, time.Now())
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create simulation output directory: %w", err)
	}
	raidWS := filepath.Join(outputDir, "raid")
	personaWS := filepath.Join(outputDir, "persona")
	if err := scenario.Copy(raidRef, raidWS); err != nil {
		return fmt.Errorf("create raid workspace: %w", err)
	}
	if err := scenario.Copy(personaRef, personaWS); err != nil {
		return fmt.Errorf("create persona workspace: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*time.Duration(*turns)+time.Minute)
	defer cancel()
	raid, err := app.Open(ctx, raidWS)
	if err != nil {
		return fmt.Errorf("open raid workspace: %w", err)
	}
	defer func() { _ = raid.Close() }()
	persona, err := app.Open(ctx, personaWS)
	if err != nil {
		return fmt.Errorf("open persona workspace: %w", err)
	}
	defer func() { _ = persona.Close() }()

	metrics := autoMetrics{
		Raid:      *raidSource,
		Persona:   *personaSource,
		StartedAt: time.Now(),
		Timeout:   *timeout,
	}
	fmt.Printf("simulating raid=%s persona=%s rounds=%d\n", *raidSource, *personaSource, *turns)
	var transcript strings.Builder
	nextPersonaInput := personaOpening
	for round := 1; round <= *turns; round++ {
		personaText, personaRendered, personaMetric, err := runAutoTurn(
			ctx, persona, nextPersonaInput, "persona", round, *timeout)
		metrics.Turns = append(metrics.Turns, personaMetric)
		if err != nil {
			return fmt.Errorf("persona round %d: %w", round, err)
		}
		raidText, raidRendered, raidMetric, err := runAutoTurn(ctx, raid, personaText, "raid", round, *timeout)
		metrics.Turns = append(metrics.Turns, raidMetric)
		if err != nil {
			return fmt.Errorf("raid round %d: %w", round, err)
		}
		fmt.Printf("--- round %d ---\npersona:\n%s\nraid:\n%s\n\n", round, personaRendered, raidRendered)
		fmt.Fprintf(&transcript, "=== Round %d ===\npersona:\n%s\nraid:\n%s\n\n",
			round, personaRendered, raidRendered)
		nextPersonaInput = raidText
	}
	metrics.FinishedAt = time.Now()
	metrics.Elapsed = metrics.FinishedAt.Sub(metrics.StartedAt)
	if err := os.WriteFile(filepath.Join(outputDir, "chat_log.txt"), []byte(transcript.String()), 0o644); err != nil {
		return err
	}
	if err := writeAutoStats(filepath.Join(outputDir, "stats.txt"), metrics); err != nil {
		return err
	}
	fmt.Printf("wrote simulation %s\n", outputDir)
	return nil
}

func runAutoTurn(
	ctx context.Context,
	a *app.App,
	input, actor string,
	round int,
	timeout time.Duration,
) (string, string, autoTurnMetric, error) {
	metric := autoTurnMetric{Actor: actor, Round: round, Input: input}
	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	observer := newTurnObserver()
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
		return "", "", metric, failure
	}
	rendered := chatfmt.Render(observer.blocks.Blocks())
	if rendered == "" {
		rendered = chatfmt.ResultText(result)
	}
	return observer.blocks.Text(), rendered, metric, nil
}

type autoTurnMetric struct {
	Actor       string
	Round       int
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

type autoMetrics struct {
	Raid       string
	Persona    string
	StartedAt  time.Time
	FinishedAt time.Time
	Elapsed    time.Duration
	Timeout    time.Duration
	Turns      []autoTurnMetric
}

func writeAutoStats(outputPath string, metrics autoMetrics) error {
	var out strings.Builder
	fmt.Fprintln(&out, "--- forge test-auto run ---")
	fmt.Fprintf(&out, "raid: %s\n", metrics.Raid)
	fmt.Fprintf(&out, "persona: %s\n", metrics.Persona)
	fmt.Fprintf(&out, "started_at: %s\n", metrics.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&out, "finished_at: %s\n", metrics.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&out, "elapsed: %s\n", metrics.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(&out, "rounds_completed: %d\n", len(metrics.Turns)/2)
	fmt.Fprintf(&out, "timeout: %s\n", metrics.Timeout)
	for _, turn := range metrics.Turns {
		fmt.Fprintf(&out, "\n--- %s round %d ---\n", turn.Actor, turn.Round)
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

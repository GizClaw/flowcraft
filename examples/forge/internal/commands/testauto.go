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
	"github.com/GizClaw/flowcraft/examples/forge/internal/story"
)

// testAutoCmd plays a raid by itself: the referee drives the table, the engine plays
// every seat, and one seat belongs to nobody, so the run asks that seat's own agent
// for the line a person would say.
//
// It is the same raid a `test` script drives, with the person replaced: a scenario
// with a human seat needs no scripted turns to be played end to end.
func testAutoCmd(args []string) error {
	flags := flag.NewFlagSet("test-auto", flag.ContinueOnError)
	raidSource := flags.String("raid", "", "raid scenario name or path")
	opening := flags.String("opening", "开始狼人杀", "the table's first line")
	player := flags.String("player", "", "the agent that plays the human seat (default: the cast's player)")
	until := flags.String("until", "获胜", "stop when the referee's answer contains this")
	timeout := flags.Duration("timeout", 4*time.Minute, "maximum duration per turn")
	rounds := flags.Int("rounds", 24, "maximum turns before the run stops")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *raidSource == "" {
		return fmt.Errorf("test-auto requires --raid\n\n%s", usage())
	}
	if *rounds <= 0 {
		return fmt.Errorf("test-auto requires --rounds > 0")
	}
	raidRef, err := scenario.Resolve("raids", *raidSource)
	if err != nil {
		return err
	}
	outputDir := testRunDir(*raidSource+"-auto", time.Now())
	workspacePath := filepath.Join(outputDir, "workspace")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create autoplay output directory: %w", err)
	}
	if err := scenario.Copy(raidRef, workspacePath); err != nil {
		return fmt.Errorf("create autoplay workspace: %w", err)
	}

	cast, err := story.Load(workspacePath)
	if err != nil {
		return err
	}
	playerAgent := strings.TrimSpace(*player)
	if playerAgent == "" {
		playerAgent = cast.Seat()
	}
	if playerAgent == "" {
		return fmt.Errorf("test-auto needs a player: pass --player, or name one in %s", story.File)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*time.Duration(*rounds)+time.Minute)
	defer cancel()
	a, err := app.Open(ctx, workspacePath)
	if err != nil {
		return fmt.Errorf("open autoplay workspace: %w", err)
	}
	defer func() { _ = a.Close() }()
	if a.InfoFor(playerAgent).Agent == "" {
		return fmt.Errorf("test-auto: %q is not deployed in %s", playerAgent, workspacePath)
	}

	metrics := autoMetrics{
		Raid:      *raidSource,
		Player:    playerAgent,
		Opening:   *opening,
		StartedAt: time.Now(),
		Timeout:   *timeout,
	}
	director := a.Info().Agent
	fmt.Printf("autoplay raid=%s referee=%s player=%s rounds<=%d\n", *raidSource, director, playerAgent, *rounds)

	var transcript strings.Builder
	input := *opening
	for round := 1; round <= *rounds; round++ {
		said, rendered, metric, err := runAutoTurn(ctx, a, director, "referee", input, round, *timeout)
		metrics.Turns = append(metrics.Turns, metric)
		if err != nil {
			return fmt.Errorf("referee round %d: %w", round, err)
		}
		fmt.Printf("--- round %d: table ---\n%s\n\n", round, rendered)
		fmt.Fprintf(&transcript, "=== Round %d (referee) ===\n%s\n\n", round, rendered)
		if strings.Contains(said, *until) {
			fmt.Printf("--- the referee ended the game after %d rounds: %s\n", round, lastLine(said))
			break
		}
		if strings.TrimSpace(said) == "" && rendered == "" {
			fmt.Printf("--- the referee asked for nothing after %d rounds\n", round)
			break
		}

		// The person's line, asked of the person's own seat: the same model, the same
		// memory, and the same brief the referee would have sent a delegate.
		answer, answerRendered, answerMetric, err := runAutoTurn(ctx, a, playerAgent, "player", said, round, *timeout)
		metrics.Turns = append(metrics.Turns, answerMetric)
		if err != nil {
			return fmt.Errorf("player round %d: %w", round, err)
		}
		fmt.Printf("--- round %d: %s ---\n%s\n\n", round, cast.Label(playerAgent), answerRendered)
		fmt.Fprintf(&transcript, "=== Round %d (%s) ===\n%s\n\n", round, cast.Label(playerAgent), answerRendered)
		input = strings.TrimSpace(answer)
		if input == "" {
			return fmt.Errorf("player round %d: the seat answered nothing", round)
		}
	}
	metrics.FinishedAt = time.Now()
	metrics.Elapsed = metrics.FinishedAt.Sub(metrics.StartedAt)
	metrics.Rounds = len(metrics.Turns) / 2
	if err := os.WriteFile(filepath.Join(outputDir, "chat_log.txt"), []byte(transcript.String()), 0o644); err != nil {
		return err
	}
	if err := writeAutoStats(filepath.Join(outputDir, "stats.txt"), metrics); err != nil {
		return err
	}
	fmt.Printf("wrote autoplay %s\n", outputDir)
	return nil
}

// runAutoTurn runs one agent's turn and reports what it said.
func runAutoTurn(
	ctx context.Context,
	a *app.App,
	agentName, actor, input string,
	round int,
	timeout time.Duration,
) (string, string, autoTurnMetric, error) {
	metric := autoTurnMetric{Actor: actor, Round: round, Input: input}
	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	observer := newTurnObserver()
	result, err := a.RunTurn(turnCtx, agentName, input, observer.observe)
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
	// Only the turn's own answer is a request for the next line. Falling back to the
	// seats' text would feed a seat's words back to the referee as if a person had
	// said them: an empty answer means the table asked for nothing.
	said := chatfmt.ResultText(result)
	if rendered == "" {
		rendered = said
	}
	return said, rendered, metric, nil
}

// lastLine is the footnote a run prints when it stops: the referee's verdict.
func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
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
	Player     string
	Opening    string
	Rounds     int
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
	fmt.Fprintf(&out, "player: %s\n", metrics.Player)
	fmt.Fprintf(&out, "opening: %s\n", metrics.Opening)
	fmt.Fprintf(&out, "started_at: %s\n", metrics.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&out, "finished_at: %s\n", metrics.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&out, "elapsed: %s\n", metrics.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(&out, "rounds_completed: %d\n", metrics.Rounds)
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

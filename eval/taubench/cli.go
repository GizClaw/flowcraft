package taubench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/internal/cliflags"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/sdk/llm"

	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

// RegisterCobra attaches the `taubench` subcommand to parent.
func RegisterCobra(parent *cobra.Command, g *cliflags.Global) {
	var (
		agentSpec         string
		customerSpec      string
		domain            string
		upstreamTasks     string
		upstreamState     string
		maxAgentTurns     int
		maxConvTurns      int
		stopToken         string
		concurrency       int
		limit             int
		perTaskTimeout    time.Duration
		includeTranscript bool
	)

	cmd := &cobra.Command{
		Use:   "taubench",
		Short: "τ-bench tool-use benchmark (single-shot + multi-turn dialog)",
		Long: `Run the τ-bench tool-use evaluation. NOT a PR gate — this drives
real LLM tool-call chains and is expensive; run it periodically or
on-demand.

Bundled domains:
  retail    NewRetailMiniDataset (5 tasks; 2 multi-turn)
  airline   NewAirlineMiniDataset (5 tasks; 1 multi-turn)
  all       both packs merged via taubench.MergeDatasets

Upstream tasks JSON (--upstream-tasks + --upstream-initial-state) is
shadow-executed against the bundled tools to derive each task's
ExpectedFinalState before the agent ever runs.

Example:
  eval taubench --agent-llm qwen:qwen-max --domain retail \
      --max-agent-turns 12 --out /tmp/taubench.json`,
		RunE: func(c *cobra.Command, _ []string) error {
			if agentSpec == "" {
				return fmt.Errorf("--agent-llm is required (e.g. qwen:qwen-max)")
			}
			notifier, err := g.Notify.Build()
			if err != nil {
				return fmt.Errorf("notify: %w", err)
			}
			agent, err := env.BuildLLM(agentSpec)
			if err != nil {
				return fmt.Errorf("--agent-llm: %w", err)
			}
			if agent == nil {
				return fmt.Errorf("--agent-llm resolved to nil; check FLOWCRAFT_<ALIAS> env var")
			}
			var customer llm.LLM
			if customerSpec != "" {
				customer, err = env.BuildLLM(customerSpec)
				if err != nil {
					return fmt.Errorf("--customer-llm: %w", err)
				}
				if customer == nil {
					return fmt.Errorf("--customer-llm resolved to nil; check FLOWCRAFT_<ALIAS> env var")
				}
			}

			var (
				ds    *Dataset
				tools map[string]Tool
			)
			switch domain {
			case "retail":
				tools = NewRetailTools()
			case "airline":
				tools = NewAirlineTools()
			case "all":
				tools = mergeToolMaps(NewRetailTools(), NewAirlineTools())
			case "sierra-retail":
				tools = NewSierraRetailTools()
			case "sierra-airline":
				tools = NewSierraAirlineTools()
			default:
				return fmt.Errorf("--domain: unknown domain %q (want retail | airline | all | sierra-retail | sierra-airline)", domain)
			}
			if upstreamTasks != "" {
				if upstreamState == "" {
					return fmt.Errorf("--upstream-initial-state is required when --upstream-tasks is set")
				}
				initState, err := LoadInitialState(upstreamState)
				if err != nil {
					return fmt.Errorf("load initial state: %w", err)
				}
				tasks, err := LoadUpstreamTasks(upstreamTasks, initState, tools, domain)
				if err != nil {
					return fmt.Errorf("load upstream tasks: %w", err)
				}
				ds = &Dataset{Name: filepath.Base(upstreamTasks), Tasks: tasks}
			} else {
				switch domain {
				case "retail":
					ds = NewRetailMiniDataset()
				case "airline":
					ds = NewAirlineMiniDataset()
				case "all":
					ds = MergeDatasets("retail+airline", NewRetailMiniDataset(), NewAirlineMiniDataset())
				case "sierra-retail", "sierra-airline":
					return fmt.Errorf("--domain %s requires --upstream-tasks and --upstream-initial-state (stage Sierra fixtures via eval/taubench/sierra/prep.py)", domain)
				}
			}

			opts := Options{
				AgentLLM:             agent,
				CustomerLLM:          customer,
				Tools:                tools,
				StopToken:            stopToken,
				MaxAgentTurns:        maxAgentTurns,
				MaxConversationTurns: maxConvTurns,
				Concurrency:          concurrency,
				LimitTasks:           limit,
				PerTaskTimeout:       perTaskTimeout,
				IncludeTranscript:    includeTranscript,
				ProgressPct:          g.Notify.ProgressPct,
				Hook: func(ctx context.Context, e Event) {
					notify.Forward(ctx, notifier, notify.Event{
						Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
					})
				},
			}

			rep, err := Run(c.Context(), ds, opts)
			if err != nil {
				return fmt.Errorf("run: %w", err)
			}
			rep.Model = agentSpec
			if err := g.WriteReport(rep); err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "  total=%d  passed=%d  pass_rate=%.3f  duration=%dms\n",
				rep.N, rep.Passed, rep.PassRate, rep.DurationMS)
			for d, dr := range rep.PerDomain {
				fmt.Fprintf(os.Stderr, "    %-10s pass_rate=%.3f (%d/%d)\n", d, dr.PassRate, dr.Passed, dr.Tasks)
			}
			for _, tr := range rep.Tasks {
				marker := "PASS"
				if !tr.Success {
					marker = "FAIL"
				}
				fmt.Fprintf(os.Stderr, "    [%s] %-32s mode=%-11s agent_turns=%d customer_turns=%d tools=%d %s\n",
					marker, tr.ID, tr.Mode, tr.AgentTurns, tr.CustomerTurns, len(tr.ToolCalls), tr.Reason)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&agentSpec, "agent-llm", "", "model under test, format provider:model (required)")
	f.StringVar(&customerSpec, "customer-llm", "", "customer-role LLM for multi-turn tasks; required iff dataset has any CustomerScenario task")
	f.StringVar(&domain, "domain", "retail", "domain pack when using bundled mini fixtures: retail | airline | all")
	f.StringVar(&upstreamTasks, "upstream-tasks", "", "path to an upstream τ-bench tasks JSON; overrides the bundled mini pack")
	f.StringVar(&upstreamState, "upstream-initial-state", "", "path to the initial DB JSON paired with --upstream-tasks; required when that flag is set")
	f.IntVar(&maxAgentTurns, "max-agent-turns", 12, "ceiling on agent Generate calls per task")
	f.IntVar(&maxConvTurns, "max-conversation-turns", 10, "ceiling on customer↔agent exchanges per multi-turn task")
	f.StringVar(&stopToken, "stop-token", "###STOP###", "substring the customer can emit to end the dialog cleanly")
	f.IntVar(&concurrency, "concurrency", 4, "parallel tasks")
	f.IntVar(&limit, "limit", 0, "run only the first N tasks (0 = all)")
	f.DurationVar(&perTaskTimeout, "per-task-timeout", 5*time.Minute, "wall-clock cap per task; 0 inherits the ambient ctx")
	f.BoolVar(&includeTranscript, "include-transcript", false, "embed per-task agent transcript in the report (adds ~few KB per task)")

	parent.AddCommand(cmd)
}

// mergeToolMaps mirrors the inline helper from the old binary. Two
// maps, flat union; tool-name collisions are a programmer error
// (retail and airline are deliberately disjoint) so we fail loud.
func mergeToolMaps(a, b map[string]Tool) map[string]Tool {
	out := make(map[string]Tool, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if _, dup := out[k]; dup {
			panic(fmt.Sprintf("taubench: duplicate tool %q across merged domains", k))
		}
		out[k] = v
	}
	return out
}

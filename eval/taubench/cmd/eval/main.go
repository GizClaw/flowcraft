// Command eval runs the τ-bench retail-domain evaluation and writes a
// JSON Report. Sibling of eval/locomo/cmd/eval, eval/simpleqa/cmd/eval
// etc.: same notify.CLIFlags surface, same Report shape ergonomics.
//
// Quick start (uses the bundled inline retail-mini task pack):
//
//	export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"qwen-max"}'
//	go run ./eval/taubench/cmd/eval \
//	    --agent-llm  qwen:qwen-max \
//	    --max-turns  12 \
//	    --out        /tmp/taubench-qwenmax.json
//
// The current binary ships only the inline retail-mini dataset (5
// hand-curated tasks). A separate converter for the upstream
// τ-bench retail / airline JSON fixtures is on the roadmap.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/eval/taubench"
	"github.com/GizClaw/flowcraft/sdk/llm"

	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

func main() {
	agentSpec := flag.String("agent-llm", "", "model under test, format provider:model (required)")
	customerSpec := flag.String("customer-llm", "", "customer-role LLM for multi-turn tasks; required iff dataset has any CustomerScenario task")
	domain := flag.String("domain", "retail", "domain pack (currently only \"retail\" — airline is on the roadmap)")
	maxAgentTurns := flag.Int("max-agent-turns", 12, "ceiling on agent Generate calls per task")
	maxConvTurns := flag.Int("max-conversation-turns", 10, "ceiling on customer↔agent exchanges per multi-turn task")
	stopToken := flag.String("stop-token", "###STOP###", "substring the customer can emit to end the dialog cleanly")
	concurrency := flag.Int("concurrency", 4, "parallel tasks")
	limit := flag.Int("limit", 0, "run only the first N tasks (0 = all)")
	perTaskTimeout := flag.Duration("per-task-timeout", 5*time.Minute, "wall-clock cap per task; 0 inherits the ambient ctx")
	includeTranscript := flag.Bool("include-transcript", false, "embed per-task agent transcript in the report (adds ~few KB per task)")
	out := flag.String("out", "", "output report path (default: stdout)")
	notifyFlags := notify.RegisterFlags(flag.CommandLine)
	flag.Parse()

	if *agentSpec == "" {
		log.Fatal("--agent-llm is required (e.g. qwen:qwen-max)")
	}
	notifier, err := notifyFlags.Build()
	if err != nil {
		log.Fatalf("notify: %v", err)
	}
	agent, err := env.BuildLLM(*agentSpec)
	if err != nil {
		log.Fatalf("--agent-llm: %v", err)
	}
	if agent == nil {
		log.Fatal("--agent-llm resolved to nil; check FLOWCRAFT_<ALIAS> env var")
	}
	var customer llm.LLM
	if *customerSpec != "" {
		customer, err = env.BuildLLM(*customerSpec)
		if err != nil {
			log.Fatalf("--customer-llm: %v", err)
		}
		if customer == nil {
			log.Fatal("--customer-llm resolved to nil; check FLOWCRAFT_<ALIAS> env var")
		}
	}

	var ds *taubench.Dataset
	var tools map[string]taubench.Tool
	switch *domain {
	case "retail":
		ds = taubench.NewRetailMiniDataset()
		tools = taubench.NewRetailTools()
	default:
		log.Fatalf("--domain: unknown domain %q (only \"retail\" is shipped)", *domain)
	}

	opts := taubench.Options{
		AgentLLM:             agent,
		CustomerLLM:          customer,
		Tools:                tools,
		StopToken:            *stopToken,
		MaxAgentTurns:        *maxAgentTurns,
		MaxConversationTurns: *maxConvTurns,
		Concurrency:          *concurrency,
		LimitTasks:           *limit,
		PerTaskTimeout:       *perTaskTimeout,
		IncludeTranscript:    *includeTranscript,
		ProgressPct:          *notifyFlags.ProgressPct,
		Hook: func(ctx context.Context, e taubench.Event) {
			notify.Forward(ctx, notifier, notify.Event{
				Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
			})
		},
	}

	rep, err := taubench.Run(context.Background(), ds, opts)
	if err != nil {
		log.Fatalf("run: %v", err)
	}
	rep.Model = *agentSpec

	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if *out == "" {
		fmt.Println(string(b))
	} else {
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			log.Fatalf("write: %v", err)
		}
		fmt.Printf("wrote %s\n", *out)
	}

	// Operator-friendly verdict.
	fmt.Fprintf(os.Stderr, "  total=%d  passed=%d  pass_rate=%.3f  duration=%dms\n",
		rep.N, rep.Passed, rep.PassRate, rep.DurationMS)
	for domain, dr := range rep.PerDomain {
		fmt.Fprintf(os.Stderr, "    %-10s pass_rate=%.3f (%d/%d)\n", domain, dr.PassRate, dr.Passed, dr.Tasks)
	}
	for _, tr := range rep.Tasks {
		marker := "PASS"
		if !tr.Success {
			marker = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "    [%s] %-32s mode=%-11s agent_turns=%d customer_turns=%d tools=%d %s\n",
			marker, tr.ID, tr.Mode, tr.AgentTurns, tr.CustomerTurns, len(tr.ToolCalls), tr.Reason)
	}
}

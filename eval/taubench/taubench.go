// Package taubench runs a [τ-bench]-style tool-use evaluation against
// a FlowCraft agent. It tests the capability vessel + sdk/kanban +
// sdk/agent are designed for: read the user's intent, call the right
// tools in the right order, mutate the world to satisfy the goal.
//
// # Two task forms
//
// A Task is scored in either of two modes, picked per-task:
//
//   - **Single-shot** — Task.Instruction is non-empty. The customer's
//     full goal is delivered to the agent as one user message and
//     the agent then chains tool calls until it stops. No customer
//     LLM is invoked. Cheap, deterministic, useful for first-cut
//     tool-call regressions.
//
//   - **Multi-turn** — Task.CustomerScenario is non-empty. A second
//     LLM (Options.CustomerLLM) roleplays the customer using the
//     scenario as private context. The customer and the agent
//     exchange messages until the customer emits the
//     Options.StopToken or either party's per-task turn cap is hit.
//     The customer NEVER sees tool calls or tool results — only the
//     agent's natural-language utterances. This is the form that
//     matches the published τ-bench numbers and tests skills like
//     handling clarifying questions and ambiguous instructions.
//
// Both modes share scoring (StateChecks + RequiredTools applied to
// the post-run World). The Report distinguishes them via per-task
// fields so a multi-domain run can mix them freely.
//
// # Cost / NOT a PR gate
//
// τ-bench burns LLM calls. The multi-turn flavour is roughly
//
//	   per_task_calls ≈ MaxConversationTurns × (1 customer + 1-3 agent)
//
// across 100+ tasks per domain, which is two orders of magnitude
// more expensive than the other suites in eval/. This suite is meant
// to run as a periodic regression (weekly / release-time / model
// swap), NOT inline on every PR. CI gates should pick a subset
// (--limit, --domain retail) at most. See the README for the
// recommended cadence per environment.
//
// We deliberately re-implement the harness rather than wrap the
// Python upstream:
//
//   - Keeps eval/ a single-binary Go module; no Python toolchain in
//     CI and no IPC glue.
//   - Lets every tool be written with FlowCraft idioms
//     (sdk/model.ToolDefinition + a plain Go handler), so the same
//     definitions can be lifted into a vessel/sdk-based agent later.
//   - The official τ-bench tasks are CC-BY-MIT-licensed JSON; we
//     bundle a small "retail mini" task pack inline so a smoke run
//     needs zero external assets.
//
// # Scoring
//
// A task passes when every [StateCheck] in its [ExpectedOutcome]
// evaluates to true after the agent has finished. The Report carries:
//
//   - PassRate            global headline
//   - PerDomain[domain]   pass-rate slice per domain
//   - Tasks               per-task verdict + reason string for failures
//
// # Roadmap
//
//   - LLM-as-customer multi-turn dialogue
//   - Airline domain tasks (retail is the first cut)
//   - Tau-bench official-task converter (load upstream JSON directly)
//
// [τ-bench]: https://arxiv.org/abs/2406.12045
package taubench

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// State is the per-task mutable "world". Concretely it's a JSON-like
// nested map (orders, products, customers, …) that tools read from
// and mutate. We use map[string]any rather than a strongly-typed
// struct because τ-bench's task fixtures are themselves a hodgepodge
// of e-commerce records and forcing a Go schema on top of them adds
// drag every time we add a new task or tool.
type State map[string]any

// Tool is one operation an agent can call during a task.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object describing the arguments
	// the LLM is expected to pass. Passed verbatim to the provider's
	// function-calling format.
	InputSchema map[string]any
	// Handler executes the tool against the task's State and returns
	// a JSON-encodable result. The handler may mutate state.
	Handler func(state State, args map[string]any) (any, error)
}

// ToolDefinition projects the tool into the sdk's wire format.
func (t Tool) ToolDefinition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: t.InputSchema,
	}
}

// StateCheck describes a predicate on the post-run State. Path is a
// dot-separated lookup (e.g. "orders.ORD-1.status"); Equals is the
// scalar value the path must resolve to. Both string and numeric
// comparisons go through `fmt.Sprint` so 42 == "42" succeeds — this
// is intentional: the LLM may stringify ids on the way out and the
// expected value in the fixture is often a literal int.
type StateCheck struct {
	Path   string
	Equals any
}

// ExpectedOutcome aggregates the success criteria for one task.
type ExpectedOutcome struct {
	StateChecks []StateCheck

	// RequiredTools, when non-empty, demands that every named tool
	// was called at least once during the run. Useful for tasks
	// where the mutation is internally undetectable from the State
	// alone (e.g. "send confirmation email" with no email log).
	RequiredTools []string
}

// Task is one τ-bench scenario. Exactly one of Instruction or
// CustomerScenario should be set; if both are set the multi-turn
// CustomerScenario path wins because it is the higher-fidelity test.
type Task struct {
	ID           string
	Domain       string
	InitialState State
	Expected     ExpectedOutcome

	// Instruction is the closed-book, single-shot goal. Used as the
	// opening (and only) user message in single-shot mode.
	Instruction string

	// CustomerScenario is private context fed to the CustomerLLM in
	// multi-turn mode. The customer is told to roleplay this scenario
	// and to emit Options.StopToken when the goal is reached or it
	// gives up. Conventional shape:
	//
	//   "You are CUST-1 (Ada Lovelace). Your reservation RES-42 needs
	//    to be cancelled because you got sick. You don't remember
	//    the reservation id at first; you'll have to ask the agent
	//    to look it up by your name."
	//
	// Leave empty to use single-shot mode.
	CustomerScenario string

	// CustomerOpening, when set, replaces the customer's first LLM
	// call. Useful when a deterministic first utterance keeps the
	// agent from wandering on benchmark warm-up. Ignored unless
	// CustomerScenario is set.
	CustomerOpening string

	// Tools restricts which tools are exposed to the agent. Empty =
	// use every tool registered for the task's domain.
	Tools []string
}

// IsMultiTurn reports whether the task should run through the
// customer-LLM dialog harness rather than the single-shot path.
func (t Task) IsMultiTurn() bool { return t.CustomerScenario != "" }

// Dataset is a collection of tasks.
type Dataset struct {
	Name  string
	Tasks []Task
}

// TaskResult is one row in the report's Tasks slice.
type TaskResult struct {
	ID        string   `json:"id"`
	Domain    string   `json:"domain"`
	Mode      string   `json:"mode"`                 // "single-shot" or "multi-turn"
	Success   bool     `json:"success"`
	Reason    string   `json:"reason,omitempty"`     // why failed (state mismatch / max turns / etc.)
	AgentTurns    int      `json:"agent_turns"`           // agent.Generate calls consumed
	CustomerTurns int      `json:"customer_turns,omitempty"` // customer.Generate calls consumed (multi-turn only)
	ToolCalls     []string `json:"tool_calls,omitempty"`  // names called, in order
	Transcript    string   `json:"transcript,omitempty"`  // human-readable log (debug only)
}

// DomainReport is the per-domain headline number.
type DomainReport struct {
	Tasks    int     `json:"tasks"`
	Passed   int     `json:"passed"`
	PassRate float64 `json:"pass_rate"`
}

// Report is the top-level JSON document.
type Report struct {
	Dataset    string                    `json:"dataset"`
	Model      string                    `json:"model"`
	StartedAt  time.Time                 `json:"started_at"`
	DurationMS int64                     `json:"duration_ms"`
	N          int                       `json:"n"`
	Passed     int                       `json:"passed"`
	PassRate   float64                   `json:"pass_rate"`
	PerDomain  map[string]*DomainReport  `json:"per_domain,omitempty"`
	Tasks      []TaskResult              `json:"tasks,omitempty"`
	Options    map[string]any            `json:"options"`
}

// Event is the canonical lifecycle event shape.
type Event struct {
	Kind   string
	Time   time.Time
	Title  string
	Body   string
	Fields map[string]string
}

type EventHook func(ctx context.Context, e Event)

// Options controls a Run.
type Options struct {
	// AgentLLM is the model under test. Required.
	AgentLLM llm.LLM

	// CustomerLLM roleplays the customer side in multi-turn tasks.
	// REQUIRED only when the dataset contains at least one task with
	// CustomerScenario set; single-shot-only datasets can leave it
	// nil. Picking a STRONG customer model (e.g. gpt-5 / qwen-max)
	// keeps the customer convincingly in-character; a weak customer
	// can sabotage even a perfect agent.
	CustomerLLM llm.LLM

	// Tools maps tool name → implementation. The set is intersected
	// with each task's Tools list (or used as-is for tasks that don't
	// specify). Required.
	Tools map[string]Tool

	// SystemPrompt prefixes every agent conversation. Default:
	// DefaultSystemPrompt.
	SystemPrompt string

	// CustomerSystemPrompt prefixes every customer conversation in
	// multi-turn mode. The literal substring "{scenario}" is
	// replaced with Task.CustomerScenario. Default:
	// DefaultCustomerSystemPrompt.
	CustomerSystemPrompt string

	// StopToken is the substring the customer can include in any
	// reply to terminate the dialog (e.g. when its goal is met or
	// it has given up). Default: "###STOP###".
	StopToken string

	// MaxAgentTurns caps the agent's Generate calls per task. Each
	// tool-call round trip counts as one agent turn regardless of
	// how many tools are dispatched in parallel. Default: 12.
	MaxAgentTurns int

	// MaxConversationTurns caps customer↔agent exchanges in
	// multi-turn mode. One exchange = (1 customer utterance + 1
	// agent reply, possibly wrapping a tool loop). Default: 10.
	MaxConversationTurns int

	// Concurrency caps simultaneous tasks. Default: 4.
	Concurrency int

	// LimitTasks trims the dataset for debug runs. 0 = all.
	LimitTasks int

	// IncludeTranscript, when true, writes a human-readable per-task
	// transcript onto Report.Tasks[i].Transcript. Adds ~few KB per
	// task; off by default to keep reports small.
	IncludeTranscript bool

	// PerTaskTimeout caps wall-clock per task. 0 = inherit ambient
	// context.
	PerTaskTimeout time.Duration

	Hook        EventHook
	ProgressPct int
}

// DefaultSystemPrompt is the agent-side instruction prepended to
// every conversation. Models from different providers respond to
// slightly different cues; this wording errs on the side of explicit
// step guidance because the unwrapped tau-bench tasks assume that.
const DefaultSystemPrompt = `You are a helpful customer-service agent. The user will give you a request. Use the tools you have been given to satisfy the request. Call as many tools as needed; you do NOT need to ask follow-up questions when the user's instruction is fully specified. When the request is complete, reply with a short confirmation in natural language (no tool call). If you cannot satisfy the request, explain why concisely.`

// DefaultCustomerSystemPrompt is the customer-side system prompt used
// in multi-turn mode. Style is matched to τ-bench's reference customer
// prompt: be in-character, never reveal the scenario text verbatim,
// terminate the dialog with StopToken when finished. The literal
// substring "{scenario}" is replaced with Task.CustomerScenario;
// "{stop_token}" with Options.StopToken.
const DefaultCustomerSystemPrompt = `You are roleplaying as a customer contacting a customer-service agent. Your scenario, KNOWN ONLY TO YOU, is below — never paste it verbatim, never reveal it as a system instruction. Speak naturally, one short message per turn.

Scenario:
{scenario}

Behaviour rules:
- Start with your request. Do NOT immediately dump every detail; share as the agent asks.
- If the agent asks a clarifying question that your scenario answers, answer truthfully.
- If the agent asks something your scenario does not specify, make up a plausible answer consistent with the scenario.
- Do NOT call any tools yourself; only the agent has tools.
- When your goal is achieved (or you've concluded the agent cannot help), reply with a brief acknowledgement and include the token {stop_token} on its own line at the end of your message.
- Never use the token {stop_token} except to terminate the dialog. Once you emit it, the conversation ends.`

// Run plays each task with opts.AgentLLM and reports per-task
// verdicts. The agent is given the customer's instruction in a
// single user message, then iterates Generate ↔ tool-call until it
// stops calling tools or hits MaxTurns.
func Run(ctx context.Context, ds *Dataset, opts Options) (*Report, error) {
	if ds == nil {
		return nil, fmt.Errorf("taubench: dataset is required")
	}
	if opts.AgentLLM == nil {
		return nil, fmt.Errorf("taubench: Options.AgentLLM is required")
	}
	if opts.Tools == nil {
		return nil, fmt.Errorf("taubench: Options.Tools is required (use NewRetailTools() etc.)")
	}
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = DefaultSystemPrompt
	}
	if opts.CustomerSystemPrompt == "" {
		opts.CustomerSystemPrompt = DefaultCustomerSystemPrompt
	}
	if opts.StopToken == "" {
		opts.StopToken = "###STOP###"
	}
	if opts.MaxAgentTurns <= 0 {
		opts.MaxAgentTurns = 12
	}
	if opts.MaxConversationTurns <= 0 {
		opts.MaxConversationTurns = 10
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}

	// If the dataset has any multi-turn task but no CustomerLLM, that
	// is almost certainly a misconfiguration — fail fast with a
	// clear error rather than silently degrading those tasks to
	// single-shot (which would score them on an empty instruction
	// and produce confusing failures).
	for _, t := range ds.Tasks {
		if t.IsMultiTurn() && opts.CustomerLLM == nil {
			return nil, fmt.Errorf("taubench: dataset contains multi-turn task %q but Options.CustomerLLM is nil", t.ID)
		}
	}

	tasks := ds.Tasks
	if opts.LimitTasks > 0 && len(tasks) > opts.LimitTasks {
		tasks = tasks[:opts.LimitTasks]
	}

	rep := &Report{
		Dataset:   ds.Name,
		StartedAt: time.Now(),
		PerDomain: map[string]*DomainReport{},
		Options: map[string]any{
			"max_agent_turns":        opts.MaxAgentTurns,
			"max_conversation_turns": opts.MaxConversationTurns,
			"concurrency":            opts.Concurrency,
			"n_tasks":                len(tasks),
			"system_prompt":          opts.SystemPrompt,
			"stop_token":             opts.StopToken,
			"has_customer_llm":       opts.CustomerLLM != nil,
		},
	}
	defer func() { rep.DurationMS = time.Since(rep.StartedAt).Milliseconds() }()

	emit := func(e Event) {
		if opts.Hook == nil {
			return
		}
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		opts.Hook(ctx, e)
	}

	multiTurnCount := 0
	for _, t := range tasks {
		if t.IsMultiTurn() {
			multiTurnCount++
		}
	}
	emit(Event{
		Kind:  "start",
		Title: ds.Name,
		Body: fmt.Sprintf("τ-bench — %d tasks (%d multi-turn / %d single-shot), max %d agent turns",
			len(tasks), multiTurnCount, len(tasks)-multiTurnCount, opts.MaxAgentTurns),
		Fields: map[string]string{
			"dataset":         ds.Name,
			"n_tasks":         fmt.Sprintf("%d", len(tasks)),
			"n_multi_turn":    fmt.Sprintf("%d", multiTurnCount),
			"max_agent_turns": fmt.Sprintf("%d", opts.MaxAgentTurns),
		},
	})

	results := make([]TaskResult, len(tasks))

	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	var doneCount int64
	total := len(tasks)

	var milestones []int64
	if total > 0 && opts.ProgressPct > 0 && opts.Hook != nil {
		for pct := opts.ProgressPct; pct <= 99; pct += opts.ProgressPct {
			ms := int64(total) * int64(pct) / 100
			if ms < 1 {
				ms = 1
			}
			milestones = append(milestones, ms)
		}
	}
	var nextMs int64

	for i, t := range tasks {
		i := i
		t := t
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			tctx := ctx
			if opts.PerTaskTimeout > 0 {
				var cancel context.CancelFunc
				tctx, cancel = context.WithTimeout(ctx, opts.PerTaskTimeout)
				defer cancel()
			}
			results[i] = runTask(tctx, t, opts)

			d := atomic.AddInt64(&doneCount, 1)
			if len(milestones) > 0 {
				idx := atomic.LoadInt64(&nextMs)
				if idx < int64(len(milestones)) && d >= milestones[idx] {
					if atomic.CompareAndSwapInt64(&nextMs, idx, idx+1) {
						pct := int64(opts.ProgressPct) * (idx + 1)
						emit(Event{
							Kind: "task_progress",
							Body: fmt.Sprintf("%d/%d (~%d%%)", d, total, pct),
							Fields: map[string]string{
								"done":  fmt.Sprintf("%d", d),
								"total": fmt.Sprintf("%d", total),
								"pct":   fmt.Sprintf("%d", pct),
							},
						})
					}
				}
			}
		}()
	}
	wg.Wait()

	// Aggregate.
	for _, r := range results {
		if r.Success {
			rep.Passed++
		}
		dr := rep.PerDomain[r.Domain]
		if dr == nil {
			dr = &DomainReport{}
			rep.PerDomain[r.Domain] = dr
		}
		dr.Tasks++
		if r.Success {
			dr.Passed++
		}
		if !opts.IncludeTranscript {
			r.Transcript = ""
		}
		rep.Tasks = append(rep.Tasks, r)
	}
	rep.N = len(results)
	if rep.N > 0 {
		rep.PassRate = float64(rep.Passed) / float64(rep.N)
	}
	for _, dr := range rep.PerDomain {
		if dr.Tasks > 0 {
			dr.PassRate = float64(dr.Passed) / float64(dr.Tasks)
		}
	}

	// Stable Tasks ordering (task ID) for deterministic JSON diffs
	// across runs that interleave goroutines differently.
	sort.Slice(rep.Tasks, func(i, j int) bool { return rep.Tasks[i].ID < rep.Tasks[j].ID })

	emit(Event{
		Kind:  "done",
		Title: ds.Name,
		Body:  fmt.Sprintf("pass_rate=%.3f (%d/%d)", rep.PassRate, rep.Passed, rep.N),
		Fields: map[string]string{
			"pass_rate": fmt.Sprintf("%.3f", rep.PassRate),
			"passed":    fmt.Sprintf("%d", rep.Passed),
			"n":         fmt.Sprintf("%d", rep.N),
			"duration":  time.Since(rep.StartedAt).Round(time.Second).String(),
		},
	})

	return rep, nil
}

// runTask dispatches a single task to the appropriate harness and
// scores the resulting state. The dispatch decision lives here (not
// inside Run) so unit tests can drive either mode directly.
func runTask(ctx context.Context, t Task, opts Options) TaskResult {
	r := TaskResult{ID: t.ID, Domain: t.Domain}
	state := cloneState(t.InitialState)

	available := availableTools(t, opts)
	defs := toolDefs(available)

	var transcript strings.Builder

	if t.IsMultiTurn() {
		r.Mode = "multi-turn"
		runDialog(ctx, t, opts, available, defs, state, &r, &transcript)
	} else {
		r.Mode = "single-shot"
		runSingleShot(ctx, t, opts, available, defs, state, &r, &transcript)
	}

	// Score regardless of how the loop terminated. If the loop set a
	// Reason already (LLM error / turn cap), we keep it; scoring then
	// runs on the partial state to surface "missed required tool"
	// alongside the primary failure.
	if missing := checkRequiredTools(t.Expected.RequiredTools, r.ToolCalls); len(missing) > 0 {
		if r.Reason == "" {
			r.Reason = fmt.Sprintf("required tools never called: %v", missing)
		}
		r.Transcript = transcript.String()
		return r
	}
	if mismatch, ok := checkStateChecks(t.Expected.StateChecks, state); !ok {
		if r.Reason == "" {
			r.Reason = "state mismatch: " + mismatch
		}
		r.Transcript = transcript.String()
		return r
	}
	if r.Reason != "" {
		// Loop bailed out but scoring would have passed; preserve
		// the loop's reason because the agent technically did not
		// "complete" the dialog properly.
		r.Transcript = transcript.String()
		return r
	}
	r.Success = true
	r.Transcript = transcript.String()
	return r
}

// availableTools intersects the task's tool whitelist with the
// run-wide registry. Empty whitelist = "use all".
func availableTools(t Task, opts Options) map[string]Tool {
	if len(t.Tools) == 0 {
		return opts.Tools
	}
	filtered := map[string]Tool{}
	for _, name := range t.Tools {
		if tool, ok := opts.Tools[name]; ok {
			filtered[name] = tool
		}
	}
	return filtered
}

// toolDefs returns a stable-ordered slice of the tool wire definitions
// so different runs send the same prompt bytes (helps cache hit on
// providers that fingerprint the tools list).
func toolDefs(tools map[string]Tool) []model.ToolDefinition {
	defs := make([]model.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		defs = append(defs, tool.ToolDefinition())
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// runSingleShot drives the closed-book path: one user message in,
// agent tool loop until empty reply, no customer LLM.
func runSingleShot(
	ctx context.Context,
	t Task,
	opts Options,
	available map[string]Tool,
	defs []model.ToolDefinition,
	state State,
	r *TaskResult,
	transcript *strings.Builder,
) {
	msgs := []llm.Message{
		{Role: model.RoleSystem, Parts: []model.Part{{Type: model.PartText, Text: opts.SystemPrompt}}},
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: t.Instruction}}},
	}
	if opts.IncludeTranscript {
		fmt.Fprintf(transcript, "USER: %s\n", t.Instruction)
	}

	_, reason := pumpAgent(ctx, opts, available, defs, state, &msgs, r, transcript, opts.MaxAgentTurns)
	if reason != "" {
		r.Reason = reason
	}
}

// runDialog drives the multi-turn path. Two parallel message lists
// (one per LLM, with mirrored roles) keep the customer scoped to
// natural-language exchanges — it never sees tool calls or tool
// results, only the agent's natural-language utterances.
func runDialog(
	ctx context.Context,
	t Task,
	opts Options,
	available map[string]Tool,
	defs []model.ToolDefinition,
	state State,
	r *TaskResult,
	transcript *strings.Builder,
) {
	customerSys := strings.ReplaceAll(opts.CustomerSystemPrompt, "{scenario}", t.CustomerScenario)
	customerSys = strings.ReplaceAll(customerSys, "{stop_token}", opts.StopToken)

	customerMsgs := []llm.Message{
		{Role: model.RoleSystem, Parts: []model.Part{{Type: model.PartText, Text: customerSys}}},
	}
	agentMsgs := []llm.Message{
		{Role: model.RoleSystem, Parts: []model.Part{{Type: model.PartText, Text: opts.SystemPrompt}}},
	}

	for conv := 0; conv < opts.MaxConversationTurns; conv++ {
		// 1. Customer speaks. First exchange may use a deterministic
		//    Opening to keep the first agent turn comparable across
		//    runs; later turns always come from the customer LLM.
		var customerText string
		if conv == 0 && t.CustomerOpening != "" {
			customerText = t.CustomerOpening
			// Seed the customer's own history with the opening so its
			// next turn knows what it just said.
			customerMsgs = append(customerMsgs,
				model.NewTextMessage(model.RoleAssistant, customerText))
		} else {
			ans, _, err := opts.CustomerLLM.Generate(ctx, customerMsgs)
			if err != nil {
				r.Reason = fmt.Sprintf("customer LLM error: %v", err)
				return
			}
			r.CustomerTurns++
			customerText = ans.Content()
			customerMsgs = append(customerMsgs, ans)
		}
		if opts.IncludeTranscript {
			fmt.Fprintf(transcript, "CUSTOMER: %s\n", customerText)
		}

		// Strip the stop token before feeding the message to the
		// agent — the agent should never see the bookkeeping token.
		stripped, stop := stripStopToken(customerText, opts.StopToken)
		if strings.TrimSpace(stripped) != "" {
			agentMsgs = append(agentMsgs,
				model.NewTextMessage(model.RoleUser, stripped))
		}
		if stop {
			// Customer ended the dialog. We do NOT call the agent
			// again — scoring runs on the current state.
			return
		}

		// 2. Agent runs its tool loop until it produces a
		//    natural-language utterance.
		agentText, reason := pumpAgent(ctx, opts, available, defs, state, &agentMsgs, r, transcript, opts.MaxAgentTurns-r.AgentTurns)
		if reason != "" {
			r.Reason = reason
			return
		}
		if strings.TrimSpace(agentText) == "" {
			// Agent kept tool-calling but never produced text. We
			// synthesise a hint so the customer has something to
			// react to; otherwise the customer LLM gets confused.
			agentText = "(agent did not reply with text)"
		}
		// 3. Relay the agent's text to the customer (as user role,
		//    because from the customer's perspective the agent IS
		//    the user of the conversation).
		customerMsgs = append(customerMsgs,
			model.NewTextMessage(model.RoleUser, agentText))
	}

	r.Reason = fmt.Sprintf("conversation did not terminate within %d turns", opts.MaxConversationTurns)
}

// pumpAgent runs the agent's tool-call inner loop until it produces a
// natural-language text reply (returned) or the per-task turn cap is
// hit (reason set, text empty). `budget` is the remaining agent-turn
// budget for THIS pump (may be smaller than opts.MaxAgentTurns in
// multi-turn mode because earlier exchanges already consumed some).
func pumpAgent(
	ctx context.Context,
	opts Options,
	available map[string]Tool,
	defs []model.ToolDefinition,
	state State,
	msgs *[]llm.Message,
	r *TaskResult,
	transcript *strings.Builder,
	budget int,
) (agentText, reason string) {
	if budget <= 0 {
		return "", fmt.Sprintf("agent did not finish within %d turns", opts.MaxAgentTurns)
	}
	for i := 0; i < budget; i++ {
		ans, _, err := opts.AgentLLM.Generate(ctx, *msgs, llm.WithTools(defs...))
		if err != nil {
			return "", fmt.Sprintf("agent LLM error: %v", err)
		}
		r.AgentTurns++

		var calls []model.ToolCall
		var text string
		for _, p := range ans.Parts {
			switch p.Type {
			case model.PartToolCall:
				if p.ToolCall != nil {
					calls = append(calls, *p.ToolCall)
				}
			case model.PartText:
				text += p.Text
			}
		}
		if text != "" && opts.IncludeTranscript {
			fmt.Fprintf(transcript, "AGENT: %s\n", text)
		}

		if len(calls) == 0 {
			return text, ""
		}
		// Agent issued tool calls — append the assistant message
		// (with the tool-call parts) and the tool result(s).
		*msgs = append(*msgs, ans)
		executeToolBatch(calls, available, state, msgs, r, transcript, opts.IncludeTranscript)
	}
	return "", fmt.Sprintf("agent did not finish within %d turns", opts.MaxAgentTurns)
}

// executeToolBatch runs every tool call in `calls` against `state`
// and appends a corresponding tool-result message for each. Unknown
// tools and bad-args JSON are surfaced as error tool results rather
// than aborting the task — this mirrors how a real provider tolerates
// agent mistakes and gives the LLM a chance to recover.
func executeToolBatch(
	calls []model.ToolCall,
	available map[string]Tool,
	state State,
	msgs *[]llm.Message,
	r *TaskResult,
	transcript *strings.Builder,
	transcripting bool,
) {
	for _, call := range calls {
		r.ToolCalls = append(r.ToolCalls, call.Name)
		tool, ok := available[call.Name]
		if !ok {
			appendToolResult(msgs, call.ID, fmt.Sprintf("error: tool %q is not registered", call.Name), true)
			if transcripting {
				fmt.Fprintf(transcript, "TOOL[%s] -> error: not registered\n", call.Name)
			}
			continue
		}
		var args map[string]any
		if call.Arguments != "" {
			if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
				appendToolResult(msgs, call.ID, fmt.Sprintf("error: invalid arguments JSON: %v", err), true)
				if transcripting {
					fmt.Fprintf(transcript, "TOOL[%s] -> error: bad args %v\n", call.Name, err)
				}
				continue
			}
		}
		out, err := tool.Handler(state, args)
		if err != nil {
			appendToolResult(msgs, call.ID, fmt.Sprintf("error: %v", err), true)
			if transcripting {
				fmt.Fprintf(transcript, "TOOL[%s](%v) -> error: %v\n", call.Name, args, err)
			}
			continue
		}
		payload, _ := json.Marshal(out)
		appendToolResult(msgs, call.ID, string(payload), false)
		if transcripting {
			fmt.Fprintf(transcript, "TOOL[%s](%v) -> %s\n", call.Name, args, payload)
		}
	}
}

// stripStopToken removes the stop token from the customer's reply if
// present, and reports whether it WAS present. We strip rather than
// keep because we don't want the agent to see the bookkeeping token.
func stripStopToken(s, token string) (stripped string, stopped bool) {
	if !strings.Contains(s, token) {
		return s, false
	}
	return strings.TrimSpace(strings.ReplaceAll(s, token, "")), true
}

// appendToolResult is a convenience for emitting the per-call tool
// result message the LLM expects after issuing a tool call.
func appendToolResult(msgs *[]llm.Message, callID, content string, isError bool) {
	*msgs = append(*msgs, llm.Message{
		Role: model.RoleTool,
		Parts: []model.Part{
			{Type: model.PartToolResult, ToolResult: &model.ToolResult{
				ToolCallID: callID,
				Content:    content,
				IsError:    isError,
			}},
		},
	})
}

func checkRequiredTools(required, called []string) []string {
	calledSet := map[string]bool{}
	for _, c := range called {
		calledSet[c] = true
	}
	var missing []string
	for _, name := range required {
		if !calledSet[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// checkStateChecks evaluates every predicate. Returns ok=true only if
// every check passes; mismatch holds the first failing check's
// human-friendly description so the report shows actionable detail.
func checkStateChecks(checks []StateCheck, state State) (mismatch string, ok bool) {
	for _, c := range checks {
		got, found := getByPath(state, c.Path)
		if !found {
			return fmt.Sprintf("path %q missing (want %v)", c.Path, c.Equals), false
		}
		if fmt.Sprint(got) != fmt.Sprint(c.Equals) {
			return fmt.Sprintf("path %q = %v, want %v", c.Path, got, c.Equals), false
		}
	}
	return "", true
}

// getByPath walks a dot-separated path through nested map[string]any.
// "orders.ORD-1.status" descends State["orders"]["ORD-1"]["status"].
// Returns (zero, false) for any missing intermediate.
func getByPath(state State, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = map[string]any(state)
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// cloneState deep-copies the per-task state so concurrent tasks don't
// share memory. Implemented via JSON round-trip — robust against
// arbitrary nesting and good enough at fixture sizes (<1 KB per task).
func cloneState(in State) State {
	if in == nil {
		return State{}
	}
	b, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return State(out)
}

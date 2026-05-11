// Package taubench runs a [τ-bench]-style tool-use evaluation against
// a FlowCraft agent. It tests the capability vessel + sdk/kanban +
// sdk/agent are designed for: read the user's intent, call the right
// tools in the right order, mutate the world to satisfy the goal.
//
// # What we ship
//
// This is a Go-native re-implementation of τ-bench's "single-turn
// instruction" variant: the customer's goal is fed to the agent in a
// single user message and the agent then chains tool calls until it
// either succeeds or hits the max-turns ceiling. The official
// τ-bench also supports an LLM-as-customer multi-turn dialog flavour;
// porting that costs another ~500 LOC of harness code and is queued
// as a follow-up.
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

// Task is one τ-bench scenario.
type Task struct {
	ID           string
	Domain       string
	Instruction  string // shown to the agent as the opening user turn
	InitialState State
	Expected     ExpectedOutcome
	// Tools restricts which tools are exposed to the agent. Empty =
	// use every tool registered for the task's domain.
	Tools []string
}

// Dataset is a collection of tasks.
type Dataset struct {
	Name  string
	Tasks []Task
}

// TaskResult is one row in the report's Tasks slice.
type TaskResult struct {
	ID         string   `json:"id"`
	Domain     string   `json:"domain"`
	Success    bool     `json:"success"`
	Reason     string   `json:"reason,omitempty"`     // why failed (state mismatch / max turns / etc.)
	NumTurns   int      `json:"num_turns"`            // LLM completions consumed
	ToolCalls  []string `json:"tool_calls,omitempty"` // names called, in order
	Transcript string   `json:"transcript,omitempty"` // human-readable log (debug only)
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

	// Tools maps tool name → implementation. The set is intersected
	// with each task's Tools list (or used as-is for tasks that don't
	// specify). Required.
	Tools map[string]Tool

	// SystemPrompt prefixes every conversation. Default: a generic
	// "be helpful, call tools when needed" instruction.
	SystemPrompt string

	// MaxTurns caps consecutive Generate calls per task. The agent
	// makes one Generate per turn; tool calls count as one turn
	// regardless of how many tools are invoked. Default: 12.
	MaxTurns int

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

// DefaultSystemPrompt is the instruction prepended to every
// conversation. Models from different providers respond to slightly
// different cues; this wording errs on the side of explicit step
// guidance because the unwrapped tau-bench tasks assume that.
const DefaultSystemPrompt = `You are a helpful customer-service agent. The user will give you a request. Use the tools you have been given to satisfy the request. Call as many tools as needed; you do NOT need to ask follow-up questions when the user's instruction is fully specified. When the request is complete, reply with a short confirmation in natural language (no tool call). If you cannot satisfy the request, explain why concisely.`

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
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = 12
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
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
			"max_turns":     opts.MaxTurns,
			"concurrency":   opts.Concurrency,
			"n_tasks":       len(tasks),
			"system_prompt": opts.SystemPrompt,
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

	emit(Event{
		Kind:  "start",
		Title: ds.Name,
		Body:  fmt.Sprintf("τ-bench — %d tasks, max %d turns", len(tasks), opts.MaxTurns),
		Fields: map[string]string{
			"dataset":   ds.Name,
			"n_tasks":   fmt.Sprintf("%d", len(tasks)),
			"max_turns": fmt.Sprintf("%d", opts.MaxTurns),
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

// runTask is the per-task agent loop. We clone the InitialState so
// tasks running in parallel cannot interfere; the cloned State is
// what the StateChecks ultimately evaluate against.
func runTask(ctx context.Context, t Task, opts Options) TaskResult {
	r := TaskResult{ID: t.ID, Domain: t.Domain}
	state := cloneState(t.InitialState)

	available := opts.Tools
	if len(t.Tools) > 0 {
		// Restrict the agent to the task's whitelisted subset.
		filtered := map[string]Tool{}
		for _, name := range t.Tools {
			if tool, ok := opts.Tools[name]; ok {
				filtered[name] = tool
			}
		}
		available = filtered
	}
	defs := make([]model.ToolDefinition, 0, len(available))
	for _, tool := range available {
		defs = append(defs, tool.ToolDefinition())
	}
	// Stable order so different runs send the same prompt bytes.
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })

	msgs := []llm.Message{
		{Role: model.RoleSystem, Parts: []model.Part{{Type: model.PartText, Text: opts.SystemPrompt}}},
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: t.Instruction}}},
	}

	var transcript strings.Builder
	if opts.IncludeTranscript {
		fmt.Fprintf(&transcript, "USER: %s\n", t.Instruction)
	}

	for turn := 0; turn < opts.MaxTurns; turn++ {
		r.NumTurns++
		ans, _, err := opts.AgentLLM.Generate(ctx, msgs, llm.WithTools(defs...))
		if err != nil {
			r.Reason = fmt.Sprintf("agent LLM error: %v", err)
			r.Transcript = transcript.String()
			return r
		}

		// Collect tool calls from the response parts.
		var calls []model.ToolCall
		var assistantText string
		for _, p := range ans.Parts {
			switch p.Type {
			case model.PartToolCall:
				if p.ToolCall != nil {
					calls = append(calls, *p.ToolCall)
				}
			case model.PartText:
				assistantText += p.Text
			}
		}
		if assistantText != "" && opts.IncludeTranscript {
			fmt.Fprintf(&transcript, "AGENT: %s\n", assistantText)
		}

		if len(calls) == 0 {
			// Agent stopped calling tools — task is "finished" from
			// its perspective. Move on to scoring.
			break
		}

		// Append the assistant message that issued the tool calls
		// (with tool-call parts intact), then run each call and
		// append the tool results so the LLM sees them on the next
		// turn.
		msgs = append(msgs, ans)
		for _, call := range calls {
			r.ToolCalls = append(r.ToolCalls, call.Name)
			tool, ok := available[call.Name]
			if !ok {
				appendToolResult(&msgs, call.ID, fmt.Sprintf("error: tool %q is not registered", call.Name), true)
				if opts.IncludeTranscript {
					fmt.Fprintf(&transcript, "TOOL[%s] -> error: not registered\n", call.Name)
				}
				continue
			}
			var args map[string]any
			if call.Arguments != "" {
				if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
					appendToolResult(&msgs, call.ID, fmt.Sprintf("error: invalid arguments JSON: %v", err), true)
					if opts.IncludeTranscript {
						fmt.Fprintf(&transcript, "TOOL[%s] -> error: bad args %v\n", call.Name, err)
					}
					continue
				}
			}
			out, err := tool.Handler(state, args)
			if err != nil {
				appendToolResult(&msgs, call.ID, fmt.Sprintf("error: %v", err), true)
				if opts.IncludeTranscript {
					fmt.Fprintf(&transcript, "TOOL[%s](%v) -> error: %v\n", call.Name, args, err)
				}
				continue
			}
			payload, _ := json.Marshal(out)
			appendToolResult(&msgs, call.ID, string(payload), false)
			if opts.IncludeTranscript {
				fmt.Fprintf(&transcript, "TOOL[%s](%v) -> %s\n", call.Name, args, payload)
			}
		}

		if r.NumTurns == opts.MaxTurns {
			r.Reason = fmt.Sprintf("agent did not finish within %d turns", opts.MaxTurns)
			r.Transcript = transcript.String()
			return r
		}
	}

	// Score.
	if missing := checkRequiredTools(t.Expected.RequiredTools, r.ToolCalls); len(missing) > 0 {
		r.Reason = fmt.Sprintf("required tools never called: %v", missing)
		r.Transcript = transcript.String()
		return r
	}
	if mismatch, ok := checkStateChecks(t.Expected.StateChecks, state); !ok {
		r.Reason = "state mismatch: " + mismatch
		r.Transcript = transcript.String()
		return r
	}
	r.Success = true
	r.Transcript = transcript.String()
	return r
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

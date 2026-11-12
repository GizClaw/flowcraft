package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	graph "github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"

	"github.com/GizClaw/flowcraft/examples/forge/internal/scenario"
)

// copyScenario materializes one scenario into a temp workspace, the same way the
// CLI does before opening it.
func copyScenario(t *testing.T, kind, source string) string {
	t.Helper()
	ref, err := scenario.Resolve(kind, source)
	if err != nil {
		t.Fatalf("resolve %s: %v", source, err)
	}
	dir := t.TempDir()
	if err := scenario.Copy(ref, dir); err != nil {
		t.Fatalf("copy scenario: %v", err)
	}
	return dir
}

// stubGenerator answers model calls without a network: the first response asks
// for a tool, later ones answer in text, which is one full ReAct cycle.
type stubGenerator struct {
	mu    sync.Mutex
	calls int
}

func (g *stubGenerator) answer() message.Message {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.calls == 1 {
		return message.Message{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{message.ToolCallPart{
				Call: message.ToolCall{
					ID:        "call-1",
					Name:      "play_music",
					Arguments: message.NewArguments(json.RawMessage(`{"title":"海阔天空"}`)),
				},
			}}},
		}
	}
	return message.NewTextMessage(message.RoleAssistant, "好，正在播放海阔天空。")
}

func (g *stubGenerator) response() inference.GenerateResponse {
	answer := g.answer()
	return inference.GenerateResponse{
		Message:      answer,
		FinishReason: inference.FinishStop,
		Usage:        usage.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

func (g *stubGenerator) Generate(context.Context, inference.GenerateRequest) (inference.GenerateResponse, error) {
	return g.response(), nil
}

func (g *stubGenerator) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	response := g.response()
	return &stubStream{response: response, text: response.Message.Content.Text()}, nil
}

// stubStream replays one text increment and one usage report.
type stubStream struct {
	response inference.GenerateResponse
	text     string
	sent     bool
}

func (s *stubStream) Next(context.Context) (inference.StreamEvent, error) {
	if s.sent {
		return inference.StreamEvent{}, io.EOF
	}
	s.sent = true
	report := s.response.Usage
	if s.text != "" {
		return inference.StreamEvent{Kind: inference.StreamText, Text: s.text}, nil
	}
	return inference.StreamEvent{Kind: inference.StreamUsage, Usage: &report}, nil
}

func (s *stubStream) Result() (inference.GenerateResponse, error) { return s.response, nil }
func (s *stubStream) Close() error                                { return nil }

func stubDrivers(generator inference.Generator) []runtime.Driver {
	return []runtime.Driver{{
		Name: "openai",
		Build: func(json.RawMessage) (inference.Provider, error) {
			return inference.Provider{Generator: generator}, nil
		},
	}}
}

func TestOpenBuildsEveryDeclaredAgent(t *testing.T) {
	dir := copyScenario(t, "raids", "../../scenarios/raids/companion")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	a, err := open(context.Background(), dir, stubDrivers(&stubGenerator{}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = a.Close() }()

	if got := a.Agents(); len(got) != 1 || got[0] != "assistant" {
		t.Fatalf("agents = %v, want [assistant]", got)
	}
	info := a.Info()
	if info.Engine != "graph" {
		t.Fatalf("engine = %q, want graph", info.Engine)
	}
	if len(info.Models) != 1 || info.Models[0] != "openai:gpt-4o-mini" {
		t.Fatalf("models = %v", info.Models)
	}
	for _, want := range []string{"play_music", "set_device_volume", "stop_playback", "files"} {
		if !contains(info.Tools, want) {
			t.Fatalf("tools = %v, want %q", info.Tools, want)
		}
	}
}

func TestRunTurnStreamsAndRunsTheToolLoop(t *testing.T) {
	dir := copyScenario(t, "raids", "../../scenarios/raids/companion")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	a, err := open(context.Background(), dir, stubDrivers(&stubGenerator{}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = a.Close() }()

	var (
		texts   []string
		results int
		toolUse int
	)
	result, err := a.RunTurn(context.Background(), "assistant", "放一首海阔天空", func(event agent.Event) {
		switch typed := event.(type) {
		case agent.Text:
			texts = append(texts, typed.Text)
		case agent.ToolCallStart:
			toolUse++
		case agent.ToolResult:
			results++
		}
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if failure := Failure(result, nil); failure != nil {
		t.Fatalf("turn failed: %v", failure)
	}
	if toolUse != 1 || results != 1 {
		t.Fatalf("tool call/result events = %d/%d, want 1/1", toolUse, results)
	}
	if got := result.Content.Text(); !strings.Contains(got, "海阔天空") {
		t.Fatalf("result text = %q", got)
	}
	// Only the answering round streams text: the first round asks for a tool
	// and says nothing.
	if len(texts) != 1 {
		t.Fatalf("streamed text = %v, want one answer", texts)
	}
	if calls := a.sim.Calls(); calls != 1 {
		t.Fatalf("simulated tool executions = %d, want 1", calls)
	}
}

func TestRunTurnResumesTheSameSession(t *testing.T) {
	dir := copyScenario(t, "raids", "../../scenarios/raids/companion")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	a, err := open(context.Background(), dir, stubDrivers(&stubGenerator{}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := a.RunTurn(context.Background(), "assistant", "第一句", nil); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := a.RunTurn(context.Background(), "assistant", "第二句", nil); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The store is the workspace's, so reopening the directory resumes the
	// session instead of starting a second one.
	reopened, err := open(context.Background(), dir, stubDrivers(&stubGenerator{}))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if _, err := reopened.RunTurn(context.Background(), "assistant", "第三句", nil); err != nil {
		t.Fatalf("turn after reopen: %v", err)
	}
}

func TestOpenReportsMissingEnvironment(t *testing.T) {
	dir := copyScenario(t, "raids", "../../scenarios/raids/companion")
	unsetEnv(t, "OPENAI_API_KEY")
	_, err := open(context.Background(), dir, stubDrivers(&stubGenerator{}))
	if err == nil {
		t.Fatal("expected the missing variable to fail the open")
	}
	if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("error %v does not name the missing variable", err)
	}
}

func TestInspectNeedsNoCredential(t *testing.T) {
	dir := copyScenario(t, "raids", "../../scenarios/raids/companion")
	unsetEnv(t, "OPENAI_API_KEY")
	infos, missing, err := Inspect(dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(infos) != 1 || infos[0].Agent != "assistant" || infos[0].Engine != "graph" {
		t.Fatalf("infos = %+v", infos)
	}
	if len(missing) != 1 || missing[0] != "OPENAI_API_KEY" {
		t.Fatalf("missing = %v", missing)
	}
}

func TestPersonaScenarioBuilds(t *testing.T) {
	dir := copyScenario(t, "personas", "../../scenarios/personas/tom")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	a, err := open(context.Background(), dir, stubDrivers(&stubGenerator{}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = a.Close() }()
	if got := a.Info().Agent; got != "tom" {
		t.Fatalf("agent = %q, want tom", got)
	}
}

// unsetEnv makes one variable absent for the duration of a test.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	previous, set := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	if set {
		t.Cleanup(func() { _ = os.Setenv(key, previous) })
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// recordingGenerator remembers what each model call was given, so a test can see
// what the engine assembled.
type recordingGenerator struct {
	mu       sync.Mutex
	contexts []int
}

func (g *recordingGenerator) Generate(context.Context, inference.GenerateRequest) (inference.GenerateResponse, error) {
	return inference.GenerateResponse{}, nil
}

func (g *recordingGenerator) GenerateStream(_ context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	g.mu.Lock()
	g.contexts = append(g.contexts, len(request.Context))
	g.mu.Unlock()
	return &stubStream{response: inference.GenerateResponse{
		Message: message.NewTextMessage(message.RoleAssistant, "ok"),
	}}, nil
}

func (g *recordingGenerator) contextSizes() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int(nil), g.contexts...)
}

// TestTurnsRememberTheConversation pins what a carried transcript buys: the second
// turn's model call sees the first turn, which is what makes a chat a chat.
func TestTurnsRememberTheConversation(t *testing.T) {
	dir := copyScenario(t, "raids", "../../scenarios/raids/companion")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	generator := &recordingGenerator{}
	a, err := open(context.Background(), dir, stubDrivers(generator))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = a.Close() }()

	for _, text := range []string{"第一句", "第二句", "第三句"} {
		if _, err := a.RunTurn(context.Background(), a.Info().Agent, text, nil); err != nil {
			t.Fatalf("turn %q: %v", text, err)
		}
	}
	sizes := generator.contextSizes()
	if len(sizes) != 3 {
		t.Fatalf("model calls = %d, want one per turn", len(sizes))
	}
	// Context is the carried transcript minus the turn being answered, plus the
	// system prompt: each turn adds the previous user line and answer.
	for turn, size := range sizes {
		if want := 2*turn + 1; size != want {
			t.Fatalf("turn %d saw %d context messages, want %d (sizes %v)",
				turn+1, size, want, sizes)
		}
	}
}

// TestOpenBuildsTheWerewolfTable is the scenario the migration is for: nine
// deployments, one referee and eight seats, each seat carrying its memory hook.
func TestOpenBuildsTheWerewolfTable(t *testing.T) {
	dir := copyScenario(t, "raids", "../../scenarios/raids/werewolf")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	a, err := open(context.Background(), dir, stubDrivers(&stubGenerator{}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = a.Close() }()

	agents := a.Agents()
	if len(agents) != 9 {
		t.Fatalf("agents = %v, want a referee and eight seats", agents)
	}
	if agents[0] != "referee" {
		t.Fatalf("first agent = %q, want the referee", agents[0])
	}
	for i := 1; i <= 8; i++ {
		if agents[i] != fmt.Sprintf("seat_%d", i) {
			t.Fatalf("agent %d = %q", i, agents[i])
		}
	}
	if info := a.Info(); info.Engine != "graph" || len(info.Models) != 1 {
		t.Fatalf("referee info = %+v", info)
	}
}

// TestStateReportsCarryThePhase: a seat's line is labelled with the phase the rules
// put it in, and the privacy of a phase comes from the rules rather than the app.
func TestStateReportsCarryThePhase(t *testing.T) {
	a := &App{}
	game := json.RawMessage(`{"phase":"night_wolf","private_phases":["night_wolf","night_seer"]}`)
	a.observeState(agent.EventExtension[graph.GraphState]{
		Name:  graph.StateName,
		Value: graph.GraphState{Step: 7, Values: map[string]json.RawMessage{"game": game}},
	})
	if got := a.Phase(); got != "night_wolf" {
		t.Fatalf("phase = %q", got)
	}
	if !a.Private("night_seer") || a.Private("day") {
		t.Fatalf("privacy = %v", a.Private("night_seer"))
	}
	// A report that carries another key says nothing about the game.
	a.observeState(agent.EventExtension[graph.GraphState]{
		Value: graph.GraphState{Step: 8, Values: map[string]json.RawMessage{"brief": json.RawMessage(`"x"`)}},
	})
	if got := a.Phase(); got != "night_wolf" {
		t.Fatalf("phase after an unrelated step = %q", got)
	}
}

// TestInspectDescribesWhoTheWorkspaceIs pins what a reader needs from a C3
// workspace without a credential: which agent directs, what each speaker is called,
// who may be delegated to, and what each one runs on. A multi-agent workspace whose
// inspect shows one anonymous agent is not inspectable at all.
func TestInspectDescribesWhoTheWorkspaceIs(t *testing.T) {
	dir := copyScenario(t, "raids", "../../scenarios/raids/werewolf")
	unsetEnv(t, "OPENAI_API_KEY")
	infos, _, err := Inspect(dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	byAgent := make(map[string]Info, len(infos))
	for _, info := range infos {
		byAgent[info.Agent] = info
	}
	referee, ok := byAgent["referee"]
	if !ok {
		t.Fatalf("no referee among %d agents", len(infos))
	}
	if !referee.Director || referee.Label != "referee" {
		t.Fatalf("referee = %+v", referee)
	}
	if len(referee.Delegates) != 8 {
		t.Fatalf("delegates = %v, want the eight seats", referee.Delegates)
	}
	if !strings.HasSuffix(referee.Graph, "graphs/referee.yaml") {
		t.Fatalf("graph = %q", referee.Graph)
	}
	if len(referee.Models) != 1 || len(referee.Tools) == 0 {
		t.Fatalf("models = %v tools = %v", referee.Models, referee.Tools)
	}
	seat, ok := byAgent["seat_1"]
	if !ok {
		t.Fatal("no seat_1")
	}
	if seat.Director || seat.Label != "1号林知" {
		t.Fatalf("seat_1 = %+v", seat)
	}
	if len(seat.Hooks) != 1 || seat.Hooks[0] != "seat-memory" {
		t.Fatalf("hooks = %v", seat.Hooks)
	}
	if len(seat.Delegates) != 0 {
		t.Fatalf("a seat delegates to nobody: %v", seat.Delegates)
	}
}

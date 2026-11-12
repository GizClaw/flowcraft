package app

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
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

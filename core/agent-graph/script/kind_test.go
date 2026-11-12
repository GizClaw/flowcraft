package script

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// fakeBackend is a stand-in language for the contract tests.
//
// Its source text is either a JSON object of writes (a "$key" string copies that
// read key) or one of two directives: "fail:" produces a runtime failure and
// "slow:" reports an exhausted budget. That is enough to exercise projection,
// validation, caching and classification without an interpreter.
type fakeBackend struct {
	name     string
	caps     Capabilities
	compile  error
	mu       sync.Mutex
	compiles int
}

func newFake() *fakeBackend {
	return &fakeBackend{
		name: "fake",
		caps: Capabilities{Deterministic: true, Terminating: true, MemoryBounded: true},
	}
}

func (f *fakeBackend) Name() string               { return f.name }
func (f *fakeBackend) Capabilities() Capabilities { return f.caps }

func (f *fakeBackend) Compile(_ context.Context, source Script) (Program, error) {
	f.mu.Lock()
	f.compiles++
	f.mu.Unlock()
	if f.compile != nil {
		return nil, f.compile
	}
	return &fakeProgram{script: source}, nil
}

func (f *fakeBackend) Run(_ context.Context, program Program, state map[string]any, _ Budget, host Host) (map[string]any, error) {
	fake, ok := program.(*fakeProgram)
	if !ok {
		return nil, fmt.Errorf("wrong program type %T", program)
	}
	text := strings.TrimSpace(fake.script.Text)
	switch {
	case strings.HasPrefix(text, "fail:"):
		return nil, errors.New(strings.TrimPrefix(text, "fail:"))
	case strings.HasPrefix(text, "slow:"):
		return nil, errdefs.BudgetExceededf("%s", strings.TrimPrefix(text, "slow:"))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("decode source: %w", err)
	}
	for key, value := range out {
		ref, ok := value.(string)
		if !ok || !strings.HasPrefix(ref, "$") {
			continue
		}
		out[key] = asLanguage(state[strings.TrimPrefix(ref, "$")])
	}
	if host != nil {
		host.Log(LogDebug, "ran "+fake.script.Name)
	}
	return out, nil
}

// asLanguage renders a projected value the way a language would see it: a
// transcript becomes a list of {role, text} objects, which is what a script
// returns when it hands one back.
func asLanguage(value any) any {
	projected, ok := value.(Messages)
	if !ok {
		return value
	}
	items := projected.Slice()
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{
			"role": string(item.Role),
			"text": item.Content.Text(),
		})
	}
	return out
}

func (f *fakeBackend) compileCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.compiles
}

type fakeProgram struct{ script Script }

func (p *fakeProgram) Version() string { return p.script.Version }

// memorySource serves script text from a map, which is what a suite or a host
// with embedded scripts wants.
type memorySource map[string]string

func (m memorySource) Load(_ context.Context, ref string) (Script, error) {
	text, ok := m[ref]
	if !ok {
		return Script{}, errdefs.NotFoundf("script: %s does not exist", ref)
	}
	return Script{Name: ref, Text: text, Version: "mem:" + ref}, nil
}

func testKind(t *testing.T, backend Backend, source Source) graph.NodeKind {
	t.Helper()
	kind, err := New(WithBackends(backend), WithSource(source))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return kind
}

// invoke builds the Invoke the engine would pass, with a declared state map.
func invoke(node, config string, values map[string]graph.Value) graph.Invoke {
	return graph.Invoke{
		Node:     node,
		Config:   json.RawMessage(config),
		State:    func(key string) graph.Value { return values[key] },
		Snapshot: func() graph.Snapshot { return graph.NewSnapshot(values) },
	}
}

func drain(t *testing.T, stream graph.NodeStream) graph.Result {
	t.Helper()
	for {
		if _, err := stream.Next(context.Background()); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("next: %v", err)
		}
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	return result
}

// declared is the state every test shares: one key of each kind a script may
// touch.
func declared() map[string]graph.Value {
	return map[string]graph.Value{
		"game":   graph.JSON([]byte(`{"phase":"night","votes":{}}`)),
		"prompt": graph.Text("你好"),
		"ready":  graph.Bool(true),
		"log":    graph.MessagesValue(graph.MessagesFromSlice([]message.Message{message.NewTextMessage(message.RoleUser, "hi")})),
	}
}

func TestInvokeWritesEveryDeclaredKind(t *testing.T) {
	kind := testKind(t, newFake(), memorySource{
		"echo": `{"game":"$game","prompt":"$prompt","ready":"$ready","log":"$log"}`,
	})
	result := drain(t, mustInvoke(t, kind, invoke("n", `{
		"backend":"fake","path":"echo",
		"reads":["game","prompt","ready","log"],
		"writes":["game","prompt","ready","log"]
	}`, declared())))

	updates := map[string]graph.Value{}
	for _, update := range result.State {
		updates[update.Key] = update.Value
	}
	if len(updates) != 4 {
		t.Fatalf("updates = %d, want one per declared key", len(updates))
	}
	if got, _ := updates["prompt"].Text(); got != "你好" {
		t.Errorf("prompt = %q", got)
	}
	if got, _ := updates["ready"].Bool(); !got {
		t.Error("ready = false")
	}
	if raw, _ := updates["game"].JSON(); !strings.Contains(string(raw), `"phase":"night"`) {
		t.Errorf("game = %s", raw)
	}
	history, err := updates["log"].Messages()
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if history.Len() != 1 || history.Slice()[0].Content.Text() != "hi" {
		t.Errorf("log = %v", history.Slice())
	}
}

func mustInvoke(t *testing.T, kind graph.NodeKind, in graph.Invoke) graph.NodeStream {
	t.Helper()
	stream, err := kind.Invoke(context.Background(), in)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return stream
}

func TestInvokeRefusesWhatTheConfigDoesNotDeclare(t *testing.T) {
	cases := []struct {
		name   string
		source string
		config string
		want   string
	}{
		{
			name:   "undeclared write",
			source: `{"prompt":"$prompt","leak":true}`,
			config: `{"backend":"fake","path":"echo","reads":["prompt"],"writes":["prompt"]}`,
			want:   "config.writes does not declare",
		},
		{
			name:   "undeclared read",
			source: `{"prompt":"x"}`,
			config: `{"backend":"fake","path":"echo","reads":["missing"],"writes":["prompt"]}`,
			want:   "is not declared by the graph",
		},
		{
			name:   "wrong kind",
			source: `{"ready":"$prompt"}`,
			config: `{"backend":"fake","path":"echo","reads":["prompt"],"writes":["ready"]}`,
			want:   "want a bool",
		},
		{
			name:   "output cap",
			source: `{"game":"$game"}`,
			config: `{"backend":"fake","path":"echo","reads":["game"],"writes":["game"],"max_output_bytes":1}`,
			want:   "max_output_bytes",
		},
		{
			name:   "write to a key the graph does not declare",
			source: `{"nope":true}`,
			config: `{"backend":"fake","path":"echo","reads":[],"writes":["nope"]}`,
			want:   "is not declared by the graph",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			kind := testKind(t, newFake(), memorySource{"echo": test.source})
			_, err := kind.Invoke(context.Background(), invoke("n", test.config, declared()))
			assertClass(t, err, errdefs.IsValidation)
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not mention %q", err, test.want)
			}
		})
	}
}

func TestInvokeClassifiesBackendFailures(t *testing.T) {
	fake := newFake()
	kind := testKind(t, fake, memorySource{"echo": "fail:rule exploded"})
	_, err := kind.Invoke(context.Background(), invoke("n",
		`{"backend":"fake","path":"echo","reads":[],"writes":[]}`, declared()))
	assertClass(t, err, errdefs.IsAborted)
	if !strings.Contains(err.Error(), "rule exploded") {
		t.Fatalf("error %v lost the backend message", err)
	}

	fake.compile = errdefs.Validationf("rule: syntax error")
	kind = testKind(t, fake, memorySource{"echo": "anything"})
	_, err = kind.Invoke(context.Background(), invoke("n",
		`{"backend":"fake","path":"echo","reads":[],"writes":[]}`, declared()))
	assertClass(t, err, errdefs.IsValidation)
}

func TestInvokeKeepsABackendCategory(t *testing.T) {
	kind := testKind(t, newFake(), memorySource{"echo": "slow:out of budget"})
	_, err := kind.Invoke(context.Background(), invoke("n",
		`{"backend":"fake","path":"echo","reads":[],"writes":[]}`, declared()))
	assertClass(t, err, errdefs.IsBudgetExceeded)
	if errdefs.IsAborted(err) {
		t.Fatalf("the node reclassified a budget failure as aborted: %v", err)
	}
}

func TestInvokeReportsAMissingSource(t *testing.T) {
	kind := testKind(t, newFake(), memorySource{})
	_, err := kind.Invoke(context.Background(), invoke("n",
		`{"backend":"fake","path":"absent","reads":[],"writes":[]}`, declared()))
	assertClass(t, err, errdefs.IsNotFound)
}

func TestCompileHappensOncePerVersion(t *testing.T) {
	fake := newFake()
	kind := testKind(t, fake, memorySource{"echo": `{"prompt":"$prompt"}`})
	in := invoke("n", `{"backend":"fake","path":"echo","reads":["prompt"],"writes":["prompt"]}`, declared())
	for i := 0; i < 5; i++ {
		drain(t, mustInvoke(t, kind, in))
	}
	if got := fake.compileCount(); got != 1 {
		t.Fatalf("compiles = %d, want 1 per source version", got)
	}
}

func TestNewRejectsAWiringMistake(t *testing.T) {
	if _, err := New(); err == nil {
		t.Fatal("a kind with no backend should not build")
	}
	kind, err := New(WithBackends(newFake(), newFake()), WithSource(memorySource{"echo": `{}`}))
	if err != nil {
		t.Fatalf("duplicate names should not fail construction: %v", err)
	}
	// The second registration is ignored, so the name resolves rather than
	// becoming a last-one-wins surprise.
	if _, err := kind.Invoke(context.Background(), invoke("n",
		`{"backend":"fake","path":"echo","reads":[],"writes":[]}`, declared())); err != nil {
		t.Fatalf("invoke: %v", err)
	}
}

func TestDecodeRefusesAnUnknownBackend(t *testing.T) {
	kind := testKind(t, newFake(), memorySource{})
	_, err := kind.Invoke(context.Background(), invoke("n",
		`{"backend":"lua","path":"x","reads":[],"writes":[]}`, declared()))
	assertClass(t, err, errdefs.IsValidation)
	if !strings.Contains(err.Error(), "offered: fake") {
		t.Fatalf("error %v does not list the registered backends", err)
	}
}

func TestDecodeRefusesAnUnhonourableBudget(t *testing.T) {
	backend := newFake()
	backend.caps = Capabilities{Deterministic: true}
	kind := testKind(t, backend, memorySource{})
	for name, config := range map[string]string{
		"steps":  `{"backend":"fake","path":"x","reads":[],"writes":[],"budget":{"max_steps":10}}`,
		"memory": `{"backend":"fake","path":"x","reads":[],"writes":[],"budget":{"memory_mib":64}}`,
		"tools":  `{"backend":"fake","path":"x","reads":[],"writes":[],"allow":["tool:echo"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := kind.Invoke(context.Background(), invoke("n", config, declared()))
			assertClass(t, err, errdefs.IsValidation)
			if !strings.Contains(err.Error(), "does not declare it") {
				t.Fatalf("error %v does not explain the missing capability", err)
			}
		})
	}
}

func TestDecodeRejectsMalformedConfig(t *testing.T) {
	kind := testKind(t, newFake(), memorySource{})
	for name, config := range map[string]string{
		"no backend":      `{"path":"x"}`,
		"no path":         `{"backend":"fake"}`,
		"unknown field":   `{"backend":"fake","path":"x","extra":1}`,
		"duplicate read":  `{"backend":"fake","path":"x","reads":["a","a"]}`,
		"duplicate write": `{"backend":"fake","path":"x","writes":["a","a"]}`,
		"empty read":      `{"backend":"fake","path":"x","reads":[""]}`,
		"negative cap":    `{"backend":"fake","path":"x","max_output_bytes":-1}`,
		"negative memory": `{"backend":"fake","path":"x","budget":{"memory_mib":-1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := kind.Invoke(context.Background(), invoke("n", config, declared()))
			assertClass(t, err, errdefs.IsValidation)
		})
	}
}

func TestHostLogIsDiscardedByDefault(t *testing.T) {
	var buf strings.Builder
	kind, err := New(WithBackends(newFake()), WithSource(memorySource{"echo": `{}`}), WithLog(&buf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, mustInvoke(t, kind, invoke("n",
		`{"backend":"fake","path":"echo","reads":[],"writes":[]}`, declared())))
	if !strings.Contains(buf.String(), "script node") {
		t.Fatalf("log = %q, want the node name", buf.String())
	}
}

func assertClass(t *testing.T, err error, check func(error) bool) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !check(err) {
		t.Fatalf("error %v has the wrong classification", err)
	}
}

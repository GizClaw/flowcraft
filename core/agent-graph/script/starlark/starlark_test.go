package starlark_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/agent-graph/script/scripttest"
	"github.com/GizClaw/flowcraft/core/agent-graph/script/starlark"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The suite's sources. The state it owns is "game" (JSON) and "answer" (text).
const (
	sourceEcho = `
def run(state):
    return {"game": state["game"], "answer": state["answer"]}
`
	sourceExtra = `
def run(state):
    return {"game": state["game"], "answer": state["answer"], "leak": True}
`
	sourceWrongKind = `
def run(state):
    return {"answer": state["game"]}
`
	sourceBroken = `
def run(state):
    fail("the rule refused this state")
`
	sourceSlow = `
def run(state):
    total = 0
    for i in range(1000000):
        total += i
    return {"answer": str(total)}
`
)

// TestBackendSatisfiesTheContract is what every backend runs: the node's
// promises, checked once and reused by the next language.
func TestBackendSatisfiesTheContract(t *testing.T) {
	scripttest.Run(t, scripttest.Fixture{
		Backend:   starlark.New(),
		Echo:      sourceEcho,
		Extra:     sourceExtra,
		WrongKind: sourceWrongKind,
		Broken:    sourceBroken,
		Slow:      sourceSlow,
	})
}

func TestBackendDeclaresItsLimits(t *testing.T) {
	backend := starlark.New()
	if got := backend.Name(); got != "starlark" {
		t.Fatalf("name = %q", got)
	}
	caps := backend.Capabilities()
	if !caps.Deterministic || !caps.Terminating {
		t.Fatalf("capabilities = %+v, want the language guarantees", caps)
	}
	if caps.MemoryBounded || caps.Isolated {
		t.Fatalf("capabilities = %+v, want no memory bound and no isolation", caps)
	}
}

func TestCompileReportsSyntaxErrorsAsValidation(t *testing.T) {
	_, err := starlark.New().Compile(context.Background(), script.Script{
		Name: "broken.star", Text: "def run(state:\n", Version: "v1",
	})
	requireClass(t, err, errdefs.IsValidation)
	if !strings.Contains(err.Error(), "broken.star") {
		t.Fatalf("error %v does not name the file", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("error %q carries more than the first line", err)
	}
}

func TestCompileRejectsAnUndefinedName(t *testing.T) {
	_, err := starlark.New().Compile(context.Background(), script.Script{
		Name: "rule.star", Text: "def run(state):\n    return missing_name\n", Version: "v1",
	})
	requireClass(t, err, errdefs.IsValidation)
	if !strings.Contains(err.Error(), "missing_name") {
		t.Fatalf("error %v does not name the undefined variable", err)
	}
}

func TestRunRequiresAnEntryPoint(t *testing.T) {
	_, err := run(t, "def helper():\n    return 1\n", map[string]any{}, script.Budget{})
	requireClass(t, err, errdefs.IsValidation)
	if !strings.Contains(err.Error(), "run(state)") {
		t.Fatalf("error %v does not describe the missing entry point", err)
	}
}

func TestRunRequiresADictResult(t *testing.T) {
	_, err := run(t, "def run(state):\n    return [1, 2]\n", map[string]any{}, script.Budget{})
	requireClass(t, err, errdefs.IsAborted)
	if !strings.Contains(err.Error(), "want a dict") {
		t.Fatalf("error %v does not describe the expected shape", err)
	}
}

func TestRunRejectsAValueItCannotWrite(t *testing.T) {
	_, err := run(t, "def run(state):\n    return {\"answer\": 1 == 1}\n", map[string]any{}, script.Budget{})
	if err != nil {
		t.Fatalf("a bool is writable, got %v", err)
	}
	_, err = run(t, "def run(state):\n    return {\"answer\": len}\n", map[string]any{}, script.Budget{})
	requireClass(t, err, errdefs.IsAborted)
}

func TestRunReportsABudgetAsItsOwnCategory(t *testing.T) {
	_, err := run(t, sourceSlow, map[string]any{}, script.Budget{MaxSteps: 100})
	requireClass(t, err, errdefs.IsBudgetExceeded)
	if !strings.Contains(err.Error(), "100 step budget") {
		t.Fatalf("error %v does not name the budget", err)
	}
	if errdefs.IsAborted(err) {
		t.Fatalf("a budget failure should not also read as an abort: %v", err)
	}
}

func TestRunHonoursContextCancellation(t *testing.T) {
	// The loop is long enough that the default budget would not stop it before
	// the caller does, so this checks the cancel path rather than the step path.
	source := "def run(state):\n    total = 0\n    for i in range(1000000000):\n        total += i\n    return {\"answer\": \"done\"}\n"
	program, err := starlark.New().Compile(context.Background(), script.Script{
		Name: "loop.star", Text: source, Version: "v1",
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	// A budget far past any real loop, so the only thing that can stop this run is
	// the caller's deadline.
	_, err = starlark.New().Run(ctx, program, map[string]any{}, script.Budget{MaxSteps: 1 << 62}, &capture{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the caller's deadline", err)
	}
}

func TestScriptLogAndPrintReachTheHost(t *testing.T) {
	host := &capture{}
	_, err := runWith(t, `
def run(state):
    log("hello", 42)
    print("from print")
    return {"answer": "ok"}
`, map[string]any{}, script.Budget{}, host)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := host.messages()
	if len(lines) < 2 {
		t.Fatalf("host saw %v, want both the log and the print", lines)
	}
	if lines[0] != "hello 42" {
		t.Fatalf("log line = %q, want the rendered arguments", lines[0])
	}
	if lines[1] != "from print" {
		t.Fatalf("print line = %q", lines[1])
	}
}

func TestRunsDoNotShareGlobals(t *testing.T) {
	backend := starlark.New()
	// Starlark has no `global`, so the mutable global is a list: the top level
	// rebuilds it on every invocation, which is exactly the property under test.
	source := `
counter = [0]

def run(state):
    counter[0] = counter[0] + 1
    return {"answer": str(counter[0])}
`
	program, err := backend.Compile(context.Background(), script.Script{
		Name: "stateful.star", Text: source, Version: "v1",
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for i := 0; i < 3; i++ {
		out, err := backend.Run(context.Background(), program, map[string]any{}, script.Budget{}, &capture{})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if out["answer"] != "1" {
			t.Fatalf("run %d saw counter %v, want every run to start from a fresh global scope", i, out["answer"])
		}
	}
}

func TestConcurrentRunsShareOneProgram(t *testing.T) {
	backend := starlark.New()
	program, err := backend.Compile(context.Background(), script.Script{
		Name: "echo.star", Text: sourceEcho, Version: "v1",
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	state := map[string]any{"game": map[string]any{"phase": "night"}, "answer": "你好"}
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := backend.Run(context.Background(), program, state, script.Budget{}, &capture{})
			if err != nil {
				failures <- err
				return
			}
			if out["answer"] != "你好" {
				failures <- errors.New("answer changed under concurrency")
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent run: %v", err)
	}
}

func TestRunsAreReproducible(t *testing.T) {
	backend := starlark.New()
	source := `
def run(state):
    votes = {}
    for seat in state["seat"]:
        votes[seat] = votes.get(seat, 0) + 1
    return {"game": {"votes": votes}}
`
	program, err := backend.Compile(context.Background(), script.Script{
		Name: "tally.star", Text: source, Version: "v1",
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	state := map[string]any{"seat": []any{"a", "b", "a", "c", "a"}}
	first, err := backend.Run(context.Background(), program, state, script.Budget{}, &capture{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	second, err := backend.Run(context.Background(), program, state, script.Budget{}, &capture{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	left, _ := json.Marshal(first)
	right, _ := json.Marshal(second)
	if string(left) != string(right) {
		t.Fatalf("two runs disagreed:\n%s\n%s", left, right)
	}
}

func BenchmarkRun(b *testing.B) {
	backend := starlark.New()
	program, err := backend.Compile(context.Background(), script.Script{
		Name: "tally.star", Version: "v1", Text: `
def run(state):
    votes = {}
    for seat in state["seat"]:
        votes[seat] = votes.get(seat, 0) + 1
    return {"game": {"votes": votes}}
`,
	})
	if err != nil {
		b.Fatalf("compile: %v", err)
	}
	state := map[string]any{"seat": []any{"a", "b", "a", "c", "a", "b", "a"}}
	host := &capture{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := backend.Run(context.Background(), program, state, script.Budget{}, host); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// run compiles and runs one source in one step, which is what every case above
// needs.
func run(t *testing.T, source string, state map[string]any, budget script.Budget) (map[string]any, error) {
	t.Helper()
	return runWith(t, source, state, budget, &capture{})
}

func runWith(t *testing.T, source string, state map[string]any, budget script.Budget, host script.Host) (map[string]any, error) {
	t.Helper()
	backend := starlark.New()
	program, err := backend.Compile(context.Background(), script.Script{
		Name: "rule.star", Text: source, Version: "v1",
	})
	if err != nil {
		return nil, err
	}
	return backend.Run(context.Background(), program, state, budget, host)
}

// capture is a host that remembers what the script said.
type capture struct {
	mu    sync.Mutex
	lines []string
}

func (c *capture) Log(_ script.LogLevel, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, msg)
}

func (c *capture) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func requireClass(t *testing.T, err error, check func(error) bool) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !check(err) {
		t.Fatalf("error %v has the wrong classification", err)
	}
}

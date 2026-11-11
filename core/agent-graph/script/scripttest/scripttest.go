// Package scripttest verifies a script backend against the node's contract.
//
// A backend is a language: it compiles a source and runs it against a projected
// state. What the node promises the graph — declared reads, declared writes,
// kinds preserved, output bounded, budgets classified — does not depend on the
// language, so it is checked once here and every backend runs the same cases
// instead of re-deriving the semantics from scratch.
//
// The suite owns the state keys it uses ("game" as JSON, "answer" as text), so a
// fixture's sources have to be written for those names.
package scripttest

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// backendName is the name every fixture registers its backend under: the suite's
// configs cannot know another name.
const backendName = "test"

// Fixture is one backend under test and the sources that drive the cases.
//
// The state is "game" (JSON) and "answer" (text); every source receives that
// state as its only argument.
type Fixture struct {
	// Backend is the engine to verify.
	Backend script.Backend
	// Echo returns the read keys unchanged: {"game": state["game"],
	// "answer": state["answer"]}. It is the source the round-trip and concurrency
	// cases run.
	Echo string
	// Extra returns the declared keys plus one undeclared key ("leak").
	Extra string
	// WrongKind returns the JSON value for "answer", which is a text key.
	WrongKind string
	// Broken fails while running.
	Broken string
	// Slow exceeds a small step budget. Required only when the backend declares
	// Capabilities.Terminating, because only then is the step budget its bound.
	Slow string
}

// Run verifies one backend. It skips a case the backend's capabilities exempt it
// from, and fails a case the backend gets wrong.
func Run(t *testing.T, fixture Fixture) {
	t.Helper()
	if fixture.Backend == nil {
		t.Fatal("scripttest: a backend is required")
	}
	sources := &static{text: map[string]string{
		"echo":       fixture.Echo,
		"extra":      fixture.Extra,
		"wrong_kind": fixture.WrongKind,
		"broken":     fixture.Broken,
		"slow":       fixture.Slow,
	}}
	kind := newKind(t, fixture, sources)

	t.Run("echo round trip", func(t *testing.T) {
		stream := invoke(t, kind, "echo", `"reads":["game","answer"],"writes":["game","answer"]`)
		updates := updatesByKey(drain(t, stream))
		if len(updates) != 2 {
			t.Fatalf("updates = %d, want game and answer", len(updates))
		}
		if got, _ := updates["answer"].Text(); got != "你好" {
			t.Fatalf("answer = %q", got)
		}
		raw, _ := updates["game"].JSON()
		if !strings.Contains(string(raw), `"phase":"night"`) {
			t.Fatalf("game = %s", raw)
		}
	})

	t.Run("undeclared write", func(t *testing.T) {
		_, err := kind.Invoke(context.Background(), input("extra",
			`"reads":["game","answer"],"writes":["game","answer"]`, nil))
		requireClass(t, err, errdefs.IsValidation, "undeclared write")
		if !strings.Contains(err.Error(), "config.writes") {
			t.Fatalf("error %v does not name the write list", err)
		}
	})

	t.Run("undeclared read", func(t *testing.T) {
		_, err := kind.Invoke(context.Background(), input("echo",
			`"reads":["missing"],"writes":["answer"]`, nil))
		requireClass(t, err, errdefs.IsValidation, "undeclared read")
	})

	t.Run("wrong kind", func(t *testing.T) {
		_, err := kind.Invoke(context.Background(), input("wrong_kind",
			`"reads":["game"],"writes":["answer"]`, nil))
		requireClass(t, err, errdefs.IsValidation, "wrong kind")
	})

	t.Run("output cap", func(t *testing.T) {
		_, err := kind.Invoke(context.Background(), input("echo",
			`"reads":["game","answer"],"writes":["game","answer"],"max_output_bytes":2`, nil))
		requireClass(t, err, errdefs.IsValidation, "output cap")
		if !strings.Contains(err.Error(), "max_output_bytes") {
			t.Fatalf("error %v does not name the cap", err)
		}
	})

	t.Run("failing script", func(t *testing.T) {
		_, err := kind.Invoke(context.Background(), input("broken", `"reads":[],"writes":[]`, nil))
		if err == nil {
			t.Fatal("a failing script must fail the node")
		}
	})

	t.Run("missing source", func(t *testing.T) {
		_, err := kind.Invoke(context.Background(), input("absent", `"reads":[],"writes":[]`, nil))
		requireClass(t, err, errdefs.IsNotFound, "missing source")
	})

	t.Run("budget", func(t *testing.T) {
		if !fixture.Backend.Capabilities().Terminating {
			t.Skip("this backend does not claim to terminate, so a step budget is not its bound")
		}
		if strings.TrimSpace(fixture.Slow) == "" {
			t.Fatal("the fixture declares Terminating but supplies no Slow source")
		}
		_, err := kind.Invoke(context.Background(), input("slow",
			`"reads":[],"writes":[],"budget":{"max_steps":50}`, nil))
		requireClass(t, err, errdefs.IsBudgetExceeded, "budget")
	})

	t.Run("concurrent invocations", func(t *testing.T) {
		request := input("echo", `"reads":["game","answer"],"writes":["game","answer"]`, nil)
		const workers = 8
		var wg sync.WaitGroup
		failures := make(chan error, workers)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := kind.Invoke(context.Background(), request); err != nil {
					failures <- err
				}
			}()
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Fatalf("concurrent invocation: %v", err)
		}
	})
}

func newKind(t *testing.T, fixture Fixture, sources script.Source) graph.NodeKind {
	t.Helper()
	kind, err := script.New(script.WithBackends(fixture.Backend), script.WithSource(sources))
	if err != nil {
		t.Fatalf("script.New: %v", err)
	}
	return kind
}

// input builds the Invoke the engine passes: the source reference, the rest of
// the node config, and optionally a replacement state.
func input(path, fields string, values map[string]graph.Value) graph.Invoke {
	if values == nil {
		values = map[string]graph.Value{
			"game":   graph.JSON([]byte(`{"phase":"night"}`)),
			"answer": graph.Text("你好"),
		}
	}
	request := graph.Invoke{
		State:    func(key string) graph.Value { return values[key] },
		Snapshot: func() graph.Snapshot { return graph.NewSnapshot(values) },
	}
	request.Node = "n"
	request.Config = []byte(
		`{"backend":"` + backendName + `","path":"` + path + `",` + fields + `}`)
	return request
}

func invoke(t *testing.T, kind graph.NodeKind, path, fields string) graph.NodeStream {
	t.Helper()
	stream, err := kind.Invoke(context.Background(), input(path, fields, nil))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return stream
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

func updatesByKey(result graph.Result) map[string]graph.Value {
	updates := make(map[string]graph.Value, len(result.State))
	for _, update := range result.State {
		updates[update.Key] = update.Value
	}
	return updates
}

func requireClass(t *testing.T, err error, check func(error) bool, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error", label)
	}
	if !check(err) {
		t.Fatalf("%s: error %v has the wrong classification", label, err)
	}
}

// static is the suite's source: text in memory, versioned by content so a case
// that changes the text cannot reuse another case's compiled program.
type static struct{ text map[string]string }

func (s *static) Load(_ context.Context, ref string) (script.Script, error) {
	text, ok := s.text[ref]
	if !ok {
		return script.Script{}, errdefs.NotFoundf("scripttest: %s does not exist", ref)
	}
	return script.Script{Name: ref, Text: text, Version: version(text)}, nil
}

func version(text string) string {
	sum := fnv.New64a()
	_, _ = sum.Write([]byte(text))
	return "fnv:" + strconv.FormatUint(sum.Sum64(), 16)
}

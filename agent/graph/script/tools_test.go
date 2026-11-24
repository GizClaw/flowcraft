package script_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/agent/graph/internal/graphtest"
	"github.com/GizClaw/flowcraft/agent/graph/script"
	"github.com/GizClaw/flowcraft/agent/graph/script/starlark"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// toolRuntime builds a real runtime whose only interesting part is one tool
// source, so the script's call goes through the same dispatch chain a tool node
// uses.
func toolRuntime(t testing.TB) *graphtest.Fixture {
	t.Helper()
	return graphtest.New(t, graphtest.Config{
		Tools: []tool.Tool{
			graphtest.Echo(),
			graphtest.Failing("boom", "the device is offline"),
		},
	})
}

// scriptKind builds the node over the Starlark backend and the test runtime.
func scriptKind(t *testing.T, rt *graphtest.Fixture, source string) graph.NodeKind {
	t.Helper()
	kind, err := script.New(
		script.WithBackends(starlark.New()),
		script.WithSource(staticSource{"rule.star": source}),
		script.WithLog(io.Discard),
	)
	if err != nil {
		t.Fatalf("script.New: %v", err)
	}
	return kind
}

type staticSource map[string]string

func (s staticSource) Load(_ context.Context, ref string) (script.Script, error) {
	text, ok := s[ref]
	if !ok {
		return script.Script{}, errdefs.NotFoundf("script: %s does not exist", ref)
	}
	return script.Script{Name: ref, Text: text, Version: "test:" + ref}, nil
}

func callScript(t *testing.T, rt *graphtest.Fixture, kind graph.NodeKind, config string) (graph.Result, error) {
	t.Helper()
	values := map[string]graph.Value{
		"game":   graph.JSON([]byte(`{"phase":"night"}`)),
		"answer": graph.Text(""),
	}
	in := graph.Invoke{
		Node:     "tally",
		Config:   json.RawMessage(config),
		State:    func(key string) graph.Value { return values[key] },
		Snapshot: func() graph.Snapshot { return graph.NewSnapshot(values) },
	}
	rt.Stamp(&in)
	stream, err := kind.Invoke(context.Background(), in)
	if err != nil {
		return graph.Result{}, err
	}
	for {
		if _, err := stream.Next(context.Background()); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return graph.Result{}, err
		}
	}
	result, err := stream.Result()
	if err != nil {
		return graph.Result{}, err
	}
	return result, nil
}

const allowEcho = `{"backend":"starlark","path":"rule.star","reads":["game"],"writes":["answer"],"allow":["tool:echo"]}`

func TestScriptCallsAnAllowedTool(t *testing.T) {
	rt := toolRuntime(t)
	kind := scriptKind(t, rt, `
def run(state):
    answer = tool("echo", {"text": "hello from the rule"})
    return {"answer": answer["text"]}
`)
	result, err := callScript(t, rt, kind, allowEcho)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	updates := map[string]graph.Value{}
	for _, update := range result.State {
		updates[update.Key] = update.Value
	}
	// The tool echoes its arguments, so the answer proves the arguments survived
	// the projection into Starlark and back out through the dispatcher.
	if got, _ := updates["answer"].Text(); !strings.Contains(got, "hello from the rule") {
		t.Fatalf("answer = %q", got)
	}

	var kinds []string
	for _, record := range result.Records {
		kinds = append(kinds, record.Kind)
	}
	if len(kinds) != 2 || kinds[0] != script.RecordRun || kinds[1] != script.RecordTool {
		t.Fatalf("records = %v, want the run and the call", kinds)
	}
	var call script.ToolRecord
	if err := json.Unmarshal(result.Records[1].Payload, &call); err != nil {
		t.Fatalf("tool record: %v", err)
	}
	if call.Name != "echo" || !call.OK || call.ArgumentsHash == "" {
		t.Fatalf("tool record = %+v", call)
	}
	var run script.RunRecord
	if err := json.Unmarshal(result.Records[0].Payload, &run); err != nil {
		t.Fatalf("run record: %v", err)
	}
	if run.Ref != "rule.star" || run.Version == "" {
		t.Fatalf("run record = %+v", run)
	}
}

func TestScriptCannotCallAToolOutsideTheAllowList(t *testing.T) {
	rt := toolRuntime(t)
	kind := scriptKind(t, rt, `
def run(state):
    return {"answer": tool("boom", {})["text"]}
`)
	_, err := callScript(t, rt, kind, allowEcho)
	if !errdefs.IsForbidden(err) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "config.allow") {
		t.Fatalf("error %v does not name the allow list", err)
	}
}

func TestScriptSeesAToolFailure(t *testing.T) {
	rt := toolRuntime(t)
	kind := scriptKind(t, rt, `
def run(state):
    return {"answer": tool("boom", {})["text"]}
`)
	_, err := callScript(t, rt, kind, `{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"],"allow":["tool:boom"]}`)
	if !errdefs.IsAborted(err) {
		t.Fatalf("error = %v, want the script to abort on a failed tool", err)
	}
	if !strings.Contains(err.Error(), "the device is offline") {
		t.Fatalf("error %v lost the tool's message", err)
	}
}

func TestScriptCallingAnUnwiredToolFails(t *testing.T) {
	rt := toolRuntime(t)
	kind := scriptKind(t, rt, `
def run(state):
    return {"answer": tool("absent", {})["text"]}
`)
	_, err := callScript(t, rt, kind, `{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"],"allow":["tool:absent"]}`)
	if err == nil {
		t.Fatal("a tool that is not wired must fail the step")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Fatalf("error %v does not name the tool", err)
	}
}

// A script that allows a tool but is handed no tool surface refuses the call where it is made, rather
// than failing inside the dispatch with an error about an unbound binding.
func TestAllowNeedsAToolSurface(t *testing.T) {
	kind, err := script.New(
		script.WithBackends(starlark.New()),
		script.WithSource(staticSource{"rule.star": `def run(state):
    return {"answer": tool("echo", {"text": "hi"})}
`}),
	)
	if err != nil {
		t.Fatalf("script.New: %v", err)
	}
	// No fixture stamps this invocation, so the node is handed the state and nothing else — the
	// same shape a run without a tool surface produces.
	values := map[string]graph.Value{"game": graph.JSON([]byte(`{}`)), "answer": graph.Text("")}
	in := graph.Invoke{
		Node:     "tally",
		Config:   json.RawMessage(allowEcho),
		State:    func(key string) graph.Value { return values[key] },
		Snapshot: func() graph.Snapshot { return graph.NewSnapshot(values) },
	}
	stream, err := kind.Invoke(context.Background(), in)
	if err == nil {
		for {
			if _, err = stream.Next(context.Background()); err != nil {
				break
			}
		}
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want a validation error", err)
	}
	if !strings.Contains(err.Error(), "no tool surface") {
		t.Fatalf("error %v does not explain the missing surface", err)
	}
}

func TestAllowEntriesAreTyped(t *testing.T) {
	rt := toolRuntime(t)
	kind := scriptKind(t, rt, `def run(state):
    return {}
`)
	for name, config := range map[string]string{
		"unprefixed":   `{"backend":"starlark","path":"rule.star","allow":["echo"]}`,
		"empty name":   `{"backend":"starlark","path":"rule.star","allow":["tool:"]}`,
		"unknown kind": `{"backend":"starlark","path":"rule.star","allow":["model:gpt"]}`,
		"duplicate":    `{"backend":"starlark","path":"rule.star","allow":["tool:echo","tool:echo"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := callScript(t, rt, kind, config); !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want a configuration error", err)
			}
		})
	}
}

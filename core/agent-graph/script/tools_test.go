package script_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/agent-graph/script/starlark"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// toolRuntime builds a real runtime whose only interesting part is one tool
// source, so the script's call goes through the same dispatch chain a tool node
// uses.
func toolRuntime(t testing.TB) *runtime.Runtime {
	t.Helper()
	root := t.TempDir()
	config := runtime.Config{
		Version: 1,
		Sandbox: runtime.SandboxConfig{Backend: runtime.BackendLocal, Writes: []string{root}},
		Files:   runtime.FilesConfig{Roots: []string{root}},
		Tools:   runtime.ToolsConfig{Enable: []string{"files"}},
	}
	rt, err := runtime.New(context.Background(), config, runtime.WithToolSource(tooltest.Source(
		tooltest.FuncTool("echo", "echo the request", func(_ context.Context, arguments string) (string, error) {
			return arguments, nil
		}),
		tooltest.FuncTool("boom", "always fails", func(context.Context, string) (string, error) {
			return "", errors.New("the device is offline")
		}),
	)))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// scriptKind builds the node over the Starlark backend and the test runtime.
func scriptKind(t *testing.T, rt *runtime.Runtime, source string) graph.NodeKind {
	t.Helper()
	kind, err := script.New(
		script.WithBackends(starlark.New()),
		script.WithRuntime(rt),
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

func callScript(t *testing.T, kind graph.NodeKind, config string) (graph.Result, error) {
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
	kind := scriptKind(t, toolRuntime(t), `
def run(state):
    answer = tool("echo", {"text": "hello from the rule"})
    return {"answer": answer["text"]}
`)
	result, err := callScript(t, kind, allowEcho)
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
	kind := scriptKind(t, toolRuntime(t), `
def run(state):
    return {"answer": tool("boom", {})["text"]}
`)
	_, err := callScript(t, kind, allowEcho)
	if !errdefs.IsForbidden(err) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "config.allow") {
		t.Fatalf("error %v does not name the allow list", err)
	}
}

func TestScriptSeesAToolFailure(t *testing.T) {
	kind := scriptKind(t, toolRuntime(t), `
def run(state):
    return {"answer": tool("boom", {})["text"]}
`)
	_, err := callScript(t, kind, `{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"],"allow":["tool:boom"]}`)
	if !errdefs.IsAborted(err) {
		t.Fatalf("error = %v, want the script to abort on a failed tool", err)
	}
	if !strings.Contains(err.Error(), "the device is offline") {
		t.Fatalf("error %v lost the tool's message", err)
	}
}

func TestScriptCallingAnUnwiredToolFails(t *testing.T) {
	kind := scriptKind(t, toolRuntime(t), `
def run(state):
    return {"answer": tool("absent", {})["text"]}
`)
	_, err := callScript(t, kind, `{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"],"allow":["tool:absent"]}`)
	if err == nil {
		t.Fatal("a tool that is not wired must fail the step")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Fatalf("error %v does not name the tool", err)
	}
}

func TestAllowNeedsARuntime(t *testing.T) {
	kind, err := script.New(
		script.WithBackends(starlark.New()),
		script.WithSource(staticSource{"rule.star": `def run(state):
    return {}
`}),
	)
	if err != nil {
		t.Fatalf("script.New: %v", err)
	}
	_, err = callScript(t, kind, allowEcho)
	if !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want a configuration error", err)
	}
	if !strings.Contains(err.Error(), "without a runtime") {
		t.Fatalf("error %v does not explain the missing runtime", err)
	}
}

func TestAllowEntriesAreTyped(t *testing.T) {
	kind := scriptKind(t, toolRuntime(t), `def run(state):
    return {}
`)
	for name, config := range map[string]string{
		"unprefixed":   `{"backend":"starlark","path":"rule.star","allow":["echo"]}`,
		"empty name":   `{"backend":"starlark","path":"rule.star","allow":["tool:"]}`,
		"unknown kind": `{"backend":"starlark","path":"rule.star","allow":["model:gpt"]}`,
		"duplicate":    `{"backend":"starlark","path":"rule.star","allow":["tool:echo","tool:echo"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := callScript(t, kind, config); !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want a configuration error", err)
			}
		})
	}
}

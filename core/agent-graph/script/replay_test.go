package script_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/goccy/go-json"
)

// invokeWith builds the Invoke the engine passes, with the replay flags and the
// recorded facts a resumed run would carry.
func invokeWith(t *testing.T, kind graph.NodeKind, config string, replay bool, records map[string][]agent.Record) (graph.Result, error) {
	t.Helper()
	values := map[string]graph.Value{
		"game":   graph.JSON([]byte(`{"phase":"night"}`)),
		"answer": graph.Text(""),
	}
	in := graph.Invoke{
		Node:   "tally",
		Config: json.RawMessage(config),
		State:  func(key string) graph.Value { return values[key] },
		Snapshot: func() graph.Snapshot {
			return graph.NewSnapshot(values)
		},
		Replay: replay,
		Records: func(name string) []agent.Record {
			return records[name]
		},
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
	return stream.Result()
}

// argumentsHash repeats the record's identity for one argument object. It is
// spelled out here on purpose: the hash is part of what a record means, so a test
// that reads records should pin the format rather than call the writer.
func argumentsHash(t testing.TB, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func toolRecord(t *testing.T, name string, index int, ok bool, text string) agent.Record {
	t.Helper()
	value := script.ToolRecord{
		Name:          name,
		ArgumentsHash: argumentsHash(t, map[string]any{}),
		Index:         index,
		OK:            ok,
		Text:          text,
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return agent.Record{Kind: script.RecordTool, Payload: raw}
}

func TestReplayAnswersAToolCallFromTheRecord(t *testing.T) {
	// The tool would fail if it ran; the record says it succeeded, so a passing
	// test proves the dispatcher was never reached.
	kind := scriptKind(t, toolRuntime(t), `
def run(state):
    return {"answer": tool("boom", {})["text"]}
`)
	config := `{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"],"allow":["tool:boom"]}`
	result, err := invokeWith(t, kind, config, true, map[string][]agent.Record{
		script.RecordTool: {toolRecord(t, "boom", 0, true, "from the record")},
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	updates := map[string]graph.Value{}
	for _, update := range result.State {
		updates[update.Key] = update.Value
	}
	if got, _ := updates["answer"].Text(); got != "from the record" {
		t.Fatalf("answer = %q, want the recorded text", got)
	}
	// A replay writes no tool fact: the answers it used are already in the task. It
	// does write the invocation line, marked as a replay, so a log can tell the two
	// apart - without it a replayed step reads exactly like a live one.
	if len(result.Records) != 1 {
		t.Fatalf("replay recorded %d facts, want the invocation line alone", len(result.Records))
	}
	record := result.Records[0]
	if record.Kind != script.RecordRun {
		t.Fatalf("record kind = %q, want %q", record.Kind, script.RecordRun)
	}
	var run script.RunRecord
	if err := json.Unmarshal(record.Payload, &run); err != nil {
		t.Fatalf("decode run record: %v", err)
	}
	if !run.Replayed {
		t.Fatalf("run record does not say it replayed: %s", record.Payload)
	}
}

func TestReplayMissFails(t *testing.T) {
	kind := scriptKind(t, toolRuntime(t), `
def run(state):
    return {"answer": tool("boom", {})["text"]}
`)
	config := `{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"],"allow":["tool:boom"]}`
	_, err := invokeWith(t, kind, config, true, nil)
	if !errdefs.IsAborted(err) {
		t.Fatalf("error = %v, want a replay miss", err)
	}
	if !strings.Contains(err.Error(), "no record of tool") {
		t.Fatalf("error %v does not explain the miss", err)
	}
}

func TestReplayReplaysAFailedCall(t *testing.T) {
	kind := scriptKind(t, toolRuntime(t), `
def run(state):
    return {"answer": tool("boom", {})["text"]}
`)
	config := `{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"],"allow":["tool:boom"]}`
	_, err := invokeWith(t, kind, config, true, map[string][]agent.Record{
		script.RecordTool: {toolRecord(t, "boom", 0, false, "the device is offline")},
	})
	if !errdefs.IsAborted(err) {
		t.Fatalf("error = %v, want the recorded failure", err)
	}
	if !strings.Contains(err.Error(), "the device is offline") {
		t.Fatalf("error %v lost the recorded message", err)
	}
}

// runRecordFor builds the fact one invocation records about itself.
func runRecordFor(t *testing.T, ref, version string) agent.Record {
	t.Helper()
	raw, err := json.Marshal(script.RunRecord{Ref: ref, Version: version})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return agent.Record{Kind: script.RecordRun, Payload: raw}
}

func TestPinnedRefusesAChangedSource(t *testing.T) {
	kind := scriptKind(t, toolRuntime(t), `
def run(state):
    return {"answer": "ok"}
`)
	config := `{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"],"version":"pinned"}`
	// The test source reports "test:rule.star"; the record says the task ran under
	// another version, so the file was edited between runs.
	_, err := invokeWith(t, kind, config, false, map[string][]agent.Record{
		script.RecordRun: {runRecordFor(t, "rule.star", "sha256:the-old-one")},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("error %v does not explain the missing version", err)
	}
}

func TestPinnedContinuesUnderTheRecordedVersion(t *testing.T) {
	kind := scriptKind(t, toolRuntime(t), `
def run(state):
    return {"answer": "ok"}
`)
	config := `{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"],"version":"pinned"}`
	if _, err := invokeWith(t, kind, config, false, map[string][]agent.Record{
		script.RecordRun: {runRecordFor(t, "rule.star", "test:rule.star")},
	}); err != nil {
		t.Fatalf("a matching version should run: %v", err)
	}
}

func TestLatestIgnoresTheRecordedVersion(t *testing.T) {
	kind := scriptKind(t, toolRuntime(t), `
def run(state):
    return {"answer": "ok"}
`)
	config := `{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"]}`
	if _, err := invokeWith(t, kind, config, false, map[string][]agent.Record{
		script.RecordRun: {runRecordFor(t, "rule.star", "sha256:the-old-one")},
	}); err != nil {
		t.Fatalf("the default policy takes the current source: %v", err)
	}
}

func TestUnknownVersionPolicyIsRefused(t *testing.T) {
	kind := scriptKind(t, toolRuntime(t), `def run(state):
    return {}
`)
	_, err := invokeWith(t, kind, `{"backend":"starlark","path":"rule.star","version":"frozen"}`, false, nil)
	if !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want a configuration error", err)
	}
	if !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("error %v does not list the supported policies", err)
	}
}

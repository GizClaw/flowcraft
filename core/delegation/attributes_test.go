package delegation

import (
	"context"
	"maps"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
)

// subagentAttributes runs one sync delegation and returns the attribute
// map the subagent's engine observed.
func subagentAttributes(t *testing.T, ctx context.Context, req Request) map[string]string {
	t.Helper()
	var got map[string]string
	engine := agent.EngineFunc(func(
		_ context.Context,
		run agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		got = maps.Clone(run.Attributes)
		return board, nil
	})
	service, _ := newSessionPathService(t, engine, nil)
	response, err := service.Delegate(ctx, req)
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	return got
}

func TestRequestAttributesReachSubagentRun(t *testing.T) {
	req := syncRequest("writer")
	req.Attributes = map[string]string{
		"sandbox.mode": "read-only",
		"tenant.id":    "acme",
	}
	got := subagentAttributes(t, context.Background(), req)
	for key, want := range req.Attributes {
		if got[key] != want {
			t.Fatalf("subagent attribute %q = %q, want %q (all: %v)",
				key, got[key], want, got)
		}
	}
}

func TestLineageWinsOverDeclaredAttributes(t *testing.T) {
	req := syncRequest("writer")
	req.Attributes = map[string]string{
		telemetry.AttrToolCallID: "spoofed-call",
		"sandbox.mode":           "read-only",
	}
	ctx := tool.WithCallID(context.Background(), "call-real")
	got := subagentAttributes(t, ctx, req)
	if got[telemetry.AttrToolCallID] != "call-real" {
		t.Fatalf("tool.call_id = %q, want the service-owned call id (all: %v)",
			got[telemetry.AttrToolCallID], got)
	}
	if got["sandbox.mode"] != "read-only" {
		t.Fatalf("declared attribute dropped by lineage merge: %v", got)
	}
}

// TestCallerRunInfoAttributesAreNotInherited pins the decision that a
// delegation carries only explicitly declared attributes: run-scoped
// payloads the caller keeps in its own attribute bag (checkpoint recovery
// blobs, in OpenCraft) must not surface inside the subagent run.
func TestCallerRunInfoAttributesAreNotInherited(t *testing.T) {
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{
		Identity:   agent.Identity{AgentID: "caller", RunID: "run-caller"},
		Attributes: map[string]string{"oc.checkpoint.request": "blob"},
	})
	got := subagentAttributes(t, ctx, syncRequest("writer"))
	if _, ok := got["oc.checkpoint.request"]; ok {
		t.Fatalf("caller run attribute leaked into the subagent: %v", got)
	}
}

func TestRequestAttributesValidate(t *testing.T) {
	valid := func(attributes map[string]string) Request {
		req := syncRequest("writer")
		req.Attributes = attributes
		return req
	}
	for name, attributes := range map[string]map[string]string{
		"nil":         nil,
		"empty":       {},
		"simple":      {"sandbox.mode": "read-only"},
		"empty value": {"flag": ""},
		"dotted keys": {"a.b.c": "1"},
		"spaced key":  {"a b": "1"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := valid(attributes).Validate(); err != nil {
				t.Fatalf("Validate = %v, want nil", err)
			}
		})
	}
	for name, attributes := range map[string]map[string]string{
		"blank key":        {"": "x"},
		"whitespace key":   {"   ": "x"},
		"padded key":       {" sandbox.mode": "read-only"},
		"trailing space":   {"sandbox.mode ": "read-only"},
		"padded value":     {"sandbox.mode": " read-only"},
		"trailing value":   {"sandbox.mode": "read-only "},
		"whitespace value": {"sandbox.mode": " "},
	} {
		t.Run(name, func(t *testing.T) {
			if err := valid(attributes).Validate(); !errdefs.IsValidation(err) {
				t.Fatalf("Validate = %v, want validation", err)
			}
		})
	}
}

func TestRunAttributesContextRoundTrip(t *testing.T) {
	if _, ok := RunAttributesFromContext(context.Background()); ok {
		t.Fatal("bare context carries run attributes")
	}
	if _, ok := RunAttributesFromContext(nil); ok {
		t.Fatal("nil context carries run attributes")
	}
	if _, ok := RunAttributesFromContext(WithRunAttributes(context.Background(), nil)); ok {
		t.Fatal("stamping nil attributes reports a value")
	}

	stamped := map[string]string{"sandbox.mode": "read-only"}
	ctx := WithRunAttributes(context.Background(), stamped)
	stamped["sandbox.mode"] = "workspace"
	got, ok := RunAttributesFromContext(ctx)
	if !ok || got["sandbox.mode"] != "read-only" {
		t.Fatalf("stamped attributes = (%v, %v), want the value at stamp time", got, ok)
	}
	got["sandbox.mode"] = "workspace"
	if again, _ := RunAttributesFromContext(ctx); again["sandbox.mode"] != "read-only" {
		t.Fatalf("reader mutated the stamped map: %v", again)
	}

	// An inner stamp replaces an outer one instead of merging: middleware
	// that narrowed the policy must not inherit the outer keys.
	inner, _ := RunAttributesFromContext(
		WithRunAttributes(ctx, map[string]string{"tenant.id": "acme"}))
	if _, ok := inner["sandbox.mode"]; ok {
		t.Fatalf("inner stamp kept the outer key: %v", inner)
	}
}

func TestCloneRequestCopiesAttributes(t *testing.T) {
	original := syncRequest("writer")
	original.Attributes = map[string]string{"sandbox.mode": "read-only"}
	clone := cloneRequest(original)
	original.Attributes["sandbox.mode"] = "workspace"
	if clone.Attributes["sandbox.mode"] != "read-only" {
		t.Fatalf("clone attributes aliased the original: %v", clone.Attributes)
	}
}

// TestIdempotentReplayWithDifferentPolicyConflicts pins that a replay
// cannot silently reuse a delegation that was admitted under a different
// execution policy: the response might have been produced under a wider
// one than the caller now intends.
func TestIdempotentReplayWithDifferentPolicyConflicts(t *testing.T) {
	service, err := NewService(boundDirectory(t, completedEngine("ok")), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()

	newRequest := func(mode string) Request {
		req := syncRequest("writer")
		req.IdempotencyKey = "policy-key"
		req.Attributes = map[string]string{"sandbox.mode": mode}
		return req
	}
	if _, err := service.Delegate(context.Background(), newRequest("read-only")); err != nil {
		t.Fatalf("first Delegate: %v", err)
	}
	replay, err := service.Delegate(context.Background(), newRequest("read-only"))
	if err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	if replay.Status != StatusSucceeded {
		t.Fatalf("identical replay status = %q, want succeeded", replay.Status)
	}
	if _, err := service.Delegate(context.Background(), newRequest("workspace")); !errdefs.IsConflict(err) {
		t.Fatalf("replay with a different policy = %v, want conflict", err)
	}
}

func TestSameDelegationRequestIgnoresLineageAttributes(t *testing.T) {
	build := func(callID, policy string) agent.Request {
		attributes := map[string]string{"sandbox.mode": policy}
		maps.Copy(attributes, lineageAttributes(callID))
		return agent.Request{Attributes: attributes}
	}
	if !sameDelegationRequest(build("call-1", "read-only"), build("call-2", "read-only")) {
		t.Fatal("a retry under a new tool call id must still resume the parked run")
	}
	if sameDelegationRequest(build("call-1", "read-only"), build("call-1", "workspace")) {
		t.Fatal("a different declared policy must not resume the parked run")
	}
}

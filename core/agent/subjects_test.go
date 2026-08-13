package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestSubjects_Format(t *testing.T) {
	cases := []struct {
		name string
		got  event.Subject
		want event.Subject
	}{
		{"run start", agent.SubjectRunStart("r1"), "agent.run.r1.start"},
		{"run end", agent.SubjectRunEnd("r1"), "agent.run.r1.end"},
		{"step start", agent.SubjectStepStart("r1", "s1"), "agent.run.r1.step.s1.start"},
		{"step complete", agent.SubjectStepComplete("r1", "s1"), "agent.run.r1.step.s1.complete"},
		{"step error", agent.SubjectStepError("r1", "s1"), "agent.run.r1.step.s1.error"},
		{"stream delta", agent.SubjectStreamDelta("r1", "s1"), "agent.run.r1.stream.s1.delta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, tc.got)
			}
			if err := tc.got.Validate(); err != nil {
				t.Fatalf("subject must be a valid event.Subject, got %v", err)
			}
		})
	}
}

func TestSubjects_DotsInIDsAreSanitised(t *testing.T) {
	// runID / actorID containing characters that are reserved in
	// event.Subject MUST be replaced so the resulting subject still
	// validates and routes predictably.
	subj := agent.SubjectStepStart("run.with.dots", "step*id")
	if err := subj.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if subj != "agent.run.run_with_dots.step.step_id.start" {
		t.Fatalf("unexpected sanitised subject: %s", subj)
	}
}

func TestPatterns_Validate(t *testing.T) {
	patterns := []event.Pattern{
		agent.PatternRun("r1"),
		agent.PatternAllRuns(),
		agent.PatternRunSteps("r1"),
		agent.PatternRunStream("r1"),
	}
	for _, p := range patterns {
		if err := p.Validate(); err != nil {
			t.Fatalf("pattern %q invalid: %v", p, err)
		}
	}
}

func TestPatterns_Matches(t *testing.T) {
	t.Run("PatternRun matches every event of one run", func(t *testing.T) {
		p := agent.PatternRun("r1")
		matches := []event.Subject{
			agent.SubjectRunStart("r1"),
			agent.SubjectRunEnd("r1"),
			agent.SubjectStepStart("r1", "s1"),
			agent.SubjectStreamDelta("r1", "s1"),
			// engine-private extension under the same prefix
			"agent.run.r1.parallel.fork",
		}
		for _, s := range matches {
			if !p.Matches(s) {
				t.Errorf("PatternRun(r1) should match %q", s)
			}
		}
		if p.Matches(agent.SubjectRunStart("r2")) {
			t.Errorf("PatternRun(r1) must not match other run")
		}
	})

	t.Run("PatternRunSteps matches step events only", func(t *testing.T) {
		p := agent.PatternRunSteps("r1")
		if !p.Matches(agent.SubjectStepStart("r1", "s1")) {
			t.Errorf("should match step.start")
		}
		if !p.Matches("agent.run.r1.step.s1.skipped") {
			t.Errorf("should match engine-private step.* extension")
		}
		if p.Matches(agent.SubjectRunStart("r1")) {
			t.Errorf("must not match run.start")
		}
		if p.Matches(agent.SubjectStreamDelta("r1", "s1")) {
			t.Errorf("must not match stream delta")
		}
	})

	t.Run("PatternRunStream matches stream deltas only", func(t *testing.T) {
		p := agent.PatternRunStream("r1")
		if !p.Matches(agent.SubjectStreamDelta("r1", "s1")) {
			t.Errorf("should match stream.delta")
		}
		if p.Matches(agent.SubjectStepStart("r1", "s1")) {
			t.Errorf("must not match step.start")
		}
	})

	t.Run("PatternAllRuns matches every run", func(t *testing.T) {
		p := agent.PatternAllRuns()
		if !p.Matches(agent.SubjectRunStart("r1")) {
			t.Errorf("should match r1")
		}
		if !p.Matches(agent.SubjectRunStart("r2")) {
			t.Errorf("should match r2")
		}
	})
}

func TestIsStreamDelta(t *testing.T) {
	yes := []event.Subject{
		agent.SubjectStreamDelta("r1", "s1"),
		agent.SubjectStreamDelta("run-with-hyphens", "actor_id"),
	}
	for _, s := range yes {
		if !agent.IsStreamDelta(s) {
			t.Errorf("expected IsStreamDelta(%q) = true", s)
		}
	}

	no := []event.Subject{
		agent.SubjectRunStart("r1"),
		agent.SubjectRunEnd("r1"),
		agent.SubjectStepStart("r1", "s1"),
		agent.SubjectStepComplete("r1", "s1"),
		agent.SubjectStepError("r1", "s1"),
		"agent.run.r1.parallel.fork", // graph-private, looks similar but not stream
		"foo.bar.delta",              // wrong prefix
		"",                           // empty
	}
	for _, s := range no {
		if agent.IsStreamDelta(s) {
			t.Errorf("expected IsStreamDelta(%q) = false", s)
		}
	}
}

func TestSubjectPrefix_Constant(t *testing.T) {
	if !strings.HasPrefix(string(agent.SubjectRunStart("r1")), agent.SubjectPrefix) {
		t.Fatalf("SubjectRunStart should start with SubjectPrefix")
	}
}

func TestSanitiseID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "_"},
		{"r1", "r1"},
		{"a.b", "a_b"},
		{"a*b", "a_b"},
		{"a>b", "a_b"},
		{"a.b*c>d", "a_b_c_d"},
		{"normal-id_123", "normal-id_123"},
	}
	for _, tc := range cases {
		if got := agent.SanitiseID(tc.in); got != tc.want {
			t.Errorf("SanitiseID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------- StreamDeltaPayload ----------

func TestDecodeStreamDelta_TextPart(t *testing.T) {
	env := mustEnvelope(t,
		agent.SubjectStreamDelta("r1", "s1"),
		map[string]any{
			"type": "part",
			"part": map[string]any{"type": "text", "text": "你好"},
		},
	)
	got, err := agent.DecodeStreamDelta(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != agent.StreamDeltaPart {
		t.Errorf("Type = %q, want part", got.Type)
	}
	text, ok := got.Part.(message.TextPart)
	if !ok || text.Text != "你好" {
		t.Errorf("Part = %#v, want TextPart 你好", got.Part)
	}
}

func TestDecodeStreamDelta_ImagePart(t *testing.T) {
	env := mustEnvelope(t,
		agent.SubjectStreamDelta("r1", "s1"),
		map[string]any{
			"type": "part",
			"part": map[string]any{
				"type": "image",
				"source": map[string]any{
					"kind":       "url",
					"url":        "https://img.example.com/a.png",
					"media_type": "image/png",
				},
			},
		},
	)
	got, err := agent.DecodeStreamDelta(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	image, ok := got.Part.(message.ImagePart)
	if !ok || image.Source.URL() != "https://img.example.com/a.png" {
		t.Errorf("Part = %#v, want ImagePart with URL source", got.Part)
	}
}

func TestDecodeStreamDelta_ToolCallPart(t *testing.T) {
	env := mustEnvelope(t,
		agent.SubjectStreamDelta("r1", "s1"),
		map[string]any{
			"type": "part",
			"part": map[string]any{
				"type": "tool_call",
				"call": map[string]any{
					"id":        "call_1",
					"name":      "search",
					"arguments": map[string]any{"q": "weather"},
				},
			},
		},
	)
	got, err := agent.DecodeStreamDelta(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	call, ok := got.Part.(message.ToolCallPart)
	if !ok || call.Call.ID != "call_1" || call.Call.Name != "search" {
		t.Errorf("Part = %#v, want ToolCallPart call_1/search", got.Part)
	}
}

func TestDecodeStreamDelta_RejectsUnknownPartType(t *testing.T) {
	env := mustEnvelope(t,
		agent.SubjectStreamDelta("r1", "s1"),
		map[string]any{
			"type": "part",
			"part": map[string]any{"type": "hologram", "text": "x"},
		},
	)
	if _, err := agent.DecodeStreamDelta(env); err == nil {
		t.Fatal("expected error for unknown part type, got nil")
	}
}

func TestDecodeStreamDelta_ParallelBranchControl(t *testing.T) {
	acceptEnv := mustEnvelope(t,
		agent.SubjectStreamDelta("r1", "branch-a"),
		map[string]any{
			"type":        "parallel_branch_accept",
			"fork_id":     "r1:start",
			"branch_id":   "branch-a",
			"speculative": true,
		},
	)
	accept, err := agent.DecodeStreamDelta(acceptEnv)
	if err != nil {
		t.Fatalf("decode accept: %v", err)
	}
	if accept.Type != agent.StreamDeltaParallelBranchAccept ||
		accept.ForkID != "r1:start" ||
		accept.BranchID != "branch-a" ||
		!accept.Speculative {
		t.Fatalf("accept payload = %+v", accept)
	}

	cancelEnv := mustEnvelope(t,
		agent.SubjectStreamDelta("r1", "branch-b"),
		map[string]any{
			"type":      "parallel_branch_cancel",
			"fork_id":   "r1:start",
			"branch_id": "branch-b",
			"reason":    "intent rejected",
		},
	)
	cancel, err := agent.DecodeStreamDelta(cancelEnv)
	if err != nil {
		t.Fatalf("decode cancel: %v", err)
	}
	if cancel.Type != agent.StreamDeltaParallelBranchCancel ||
		cancel.ForkID != "r1:start" ||
		cancel.BranchID != "branch-b" ||
		cancel.Reason != "intent rejected" {
		t.Fatalf("cancel payload = %+v", cancel)
	}
}

func TestDecodeStreamDelta_EmptyPayload(t *testing.T) {
	env := event.Envelope{Subject: agent.SubjectStreamDelta("r1", "s1")}
	if _, err := agent.DecodeStreamDelta(env); err == nil {
		t.Fatal("expected error for empty payload, got nil")
	}
}

func TestDecodeStreamDelta_BadJSON(t *testing.T) {
	env := event.Envelope{
		Subject: agent.SubjectStreamDelta("r1", "s1"),
		Payload: json.RawMessage(`{not json`),
	}
	if _, err := agent.DecodeStreamDelta(env); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

// ---------- helpers ----------

func mustEnvelope(t *testing.T, subject event.Subject, payload any) event.Envelope {
	t.Helper()
	env, err := event.NewEnvelope(context.Background(), subject, payload)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	return env
}

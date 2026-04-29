package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
)

func TestSubjects_Format(t *testing.T) {
	cases := []struct {
		name string
		got  event.Subject
		want event.Subject
	}{
		{"run start", engine.SubjectRunStart("r1"), "engine.run.r1.start"},
		{"run end", engine.SubjectRunEnd("r1"), "engine.run.r1.end"},
		{"step start", engine.SubjectStepStart("r1", "s1"), "engine.run.r1.step.s1.start"},
		{"step complete", engine.SubjectStepComplete("r1", "s1"), "engine.run.r1.step.s1.complete"},
		{"step error", engine.SubjectStepError("r1", "s1"), "engine.run.r1.step.s1.error"},
		{"stream delta", engine.SubjectStreamDelta("r1", "s1"), "engine.run.r1.stream.s1.delta"},
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
	subj := engine.SubjectStepStart("run.with.dots", "step*id")
	if err := subj.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if subj != "engine.run.run_with_dots.step.step_id.start" {
		t.Fatalf("unexpected sanitised subject: %s", subj)
	}
}

func TestPatterns_Validate(t *testing.T) {
	patterns := []event.Pattern{
		engine.PatternRun("r1"),
		engine.PatternAllRuns(),
		engine.PatternRunSteps("r1"),
		engine.PatternRunStream("r1"),
	}
	for _, p := range patterns {
		if err := p.Validate(); err != nil {
			t.Fatalf("pattern %q invalid: %v", p, err)
		}
	}
}

func TestPatterns_Matches(t *testing.T) {
	t.Run("PatternRun matches every event of one run", func(t *testing.T) {
		p := engine.PatternRun("r1")
		matches := []event.Subject{
			engine.SubjectRunStart("r1"),
			engine.SubjectRunEnd("r1"),
			engine.SubjectStepStart("r1", "s1"),
			engine.SubjectStreamDelta("r1", "s1"),
			// engine-private extension under the same prefix
			"engine.run.r1.parallel.fork",
		}
		for _, s := range matches {
			if !p.Matches(s) {
				t.Errorf("PatternRun(r1) should match %q", s)
			}
		}
		if p.Matches(engine.SubjectRunStart("r2")) {
			t.Errorf("PatternRun(r1) must not match other run")
		}
	})

	t.Run("PatternRunSteps matches step events only", func(t *testing.T) {
		p := engine.PatternRunSteps("r1")
		if !p.Matches(engine.SubjectStepStart("r1", "s1")) {
			t.Errorf("should match step.start")
		}
		if !p.Matches("engine.run.r1.step.s1.skipped") {
			t.Errorf("should match engine-private step.* extension")
		}
		if p.Matches(engine.SubjectRunStart("r1")) {
			t.Errorf("must not match run.start")
		}
		if p.Matches(engine.SubjectStreamDelta("r1", "s1")) {
			t.Errorf("must not match stream delta")
		}
	})

	t.Run("PatternRunStream matches stream deltas only", func(t *testing.T) {
		p := engine.PatternRunStream("r1")
		if !p.Matches(engine.SubjectStreamDelta("r1", "s1")) {
			t.Errorf("should match stream.delta")
		}
		if p.Matches(engine.SubjectStepStart("r1", "s1")) {
			t.Errorf("must not match step.start")
		}
	})

	t.Run("PatternAllRuns matches every run", func(t *testing.T) {
		p := engine.PatternAllRuns()
		if !p.Matches(engine.SubjectRunStart("r1")) {
			t.Errorf("should match r1")
		}
		if !p.Matches(engine.SubjectRunStart("r2")) {
			t.Errorf("should match r2")
		}
	})
}

func TestIsStreamDelta(t *testing.T) {
	yes := []event.Subject{
		engine.SubjectStreamDelta("r1", "s1"),
		engine.SubjectStreamDelta("run-with-hyphens", "actor_id"),
	}
	for _, s := range yes {
		if !engine.IsStreamDelta(s) {
			t.Errorf("expected IsStreamDelta(%q) = true", s)
		}
	}

	no := []event.Subject{
		engine.SubjectRunStart("r1"),
		engine.SubjectRunEnd("r1"),
		engine.SubjectStepStart("r1", "s1"),
		engine.SubjectStepComplete("r1", "s1"),
		engine.SubjectStepError("r1", "s1"),
		"engine.run.r1.parallel.fork", // graph-private, looks similar but not stream
		"foo.bar.delta",               // wrong prefix
		"",                            // empty
	}
	for _, s := range no {
		if engine.IsStreamDelta(s) {
			t.Errorf("expected IsStreamDelta(%q) = false", s)
		}
	}
}

func TestSubjectPrefix_Constant(t *testing.T) {
	if !strings.HasPrefix(string(engine.SubjectRunStart("r1")), engine.SubjectPrefix) {
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
		if got := engine.SanitiseID(tc.in); got != tc.want {
			t.Errorf("SanitiseID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------- StreamDeltaPayload ----------

func TestDecodeStreamDelta_Token(t *testing.T) {
	env := mustEnvelope(t,
		engine.SubjectStreamDelta("r1", "s1"),
		map[string]any{"type": "token", "content": "你好"},
	)
	got, err := engine.DecodeStreamDelta(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != engine.StreamDeltaToken {
		t.Errorf("Type = %q, want token", got.Type)
	}
	if got.Content != "你好" {
		t.Errorf("Content = %q, want 你好", got.Content)
	}
}

func TestDecodeStreamDelta_ToolCall(t *testing.T) {
	env := mustEnvelope(t,
		engine.SubjectStreamDelta("r1", "s1"),
		map[string]any{
			"type":      "tool_call",
			"id":        "call_1",
			"name":      "search",
			"arguments": map[string]any{"q": "weather"},
		},
	)
	got, err := engine.DecodeStreamDelta(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != engine.StreamDeltaToolCall {
		t.Errorf("Type = %q, want tool_call", got.Type)
	}
	if got.ID != "call_1" || got.Name != "search" {
		t.Errorf("ID/Name = %q/%q, want call_1/search", got.ID, got.Name)
	}
	args, ok := got.Arguments.(map[string]any)
	if !ok || args["q"] != "weather" {
		t.Errorf("Arguments = %#v, want map with q=weather", got.Arguments)
	}
}

func TestDecodeStreamDelta_ToolResult(t *testing.T) {
	env := mustEnvelope(t,
		engine.SubjectStreamDelta("r1", "s1"),
		map[string]any{
			"type":         "tool_result",
			"tool_call_id": "call_1",
			"name":         "search",
			"content":      "sunny",
			"is_error":     false,
		},
	)
	got, err := engine.DecodeStreamDelta(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != engine.StreamDeltaToolResult {
		t.Errorf("Type = %q, want tool_result", got.Type)
	}
	if got.ToolCallID != "call_1" || got.Content != "sunny" {
		t.Errorf("got %#v", got)
	}
	if got.IsError {
		t.Errorf("IsError = true, want false")
	}
}

func TestDecodeStreamDelta_CancelledToolResult(t *testing.T) {
	env := mustEnvelope(t,
		engine.SubjectStreamDelta("r1", "s1"),
		map[string]any{
			"type":         "tool_result",
			"tool_call_id": "call_1",
			"content":      "[cancelled by interrupt]",
			"is_error":     true,
			"cancelled":    true,
		},
	)
	got, err := engine.DecodeStreamDelta(env)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Cancelled || !got.IsError {
		t.Errorf("expected Cancelled and IsError both true, got %#v", got)
	}
}

func TestDecodeStreamDelta_EmptyPayload(t *testing.T) {
	env := event.Envelope{Subject: engine.SubjectStreamDelta("r1", "s1")}
	if _, err := engine.DecodeStreamDelta(env); err == nil {
		t.Fatal("expected error for empty payload, got nil")
	}
}

func TestDecodeStreamDelta_BadJSON(t *testing.T) {
	env := event.Envelope{
		Subject: engine.SubjectStreamDelta("r1", "s1"),
		Payload: json.RawMessage(`{not json`),
	}
	if _, err := engine.DecodeStreamDelta(env); err == nil {
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

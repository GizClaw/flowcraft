package vesselquality

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"

	"github.com/GizClaw/flowcraft/tests/quality/vessel/fakellm"
)

// TestHistoryAccess_ReadWrite_AppendsTurns asserts the default
// HistoryAccess (ReadWrite when Spec.History is set) persists
// both the user turn and the assistant reply into the shared
// transcript. Two sequential Calls under the same ContextID
// MUST observe the prior turn during the second LLM call.
func TestHistoryAccess_ReadWrite_AppendsTurns(t *testing.T) {
	t.Parallel()
	fake := fakellm.New([]fakellm.Step{
		{Text: "first reply"},
		{Text: "second reply (saw history)"},
	}, fakellm.WithRepeatLast())

	vs := spec.Spec{
		ID: "v-hist-rw",
		Agents: []spec.Agent{
			{Name: "primary", HistoryAccess: spec.HistoryAccessReadWrite},
		},
		History: &spec.History{Kind: "buffer", MaxMessages: 50},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{"primary": fake}, 2)),
	)

	for i, q := range []string{"first question", "second question"} {
		_, err := c.Call(context.Background(), "primary", agent.Request{
			ContextID: "conv-rw",
			Message:   model.NewTextMessage(model.RoleUser, q),
		})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	// On the second LLM call the seeder should have prepended
	// the prior {user, assistant} pair PLUS the new user turn.
	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("LLM calls = %d, want 2", len(calls))
	}
	second := calls[1].Messages
	// Expected layout: [user1, assistant1, user2]. We do not
	// assert exact length (history may add tool turns in
	// engines that use them), but the first user1 + a previous
	// assistant + the trailing user2 MUST all be present.
	var sawUser1, sawAssistant1, sawUser2 bool
	for _, m := range second {
		c := m.Content()
		switch m.Role {
		case model.RoleUser:
			if strings.Contains(c, "first question") {
				sawUser1 = true
			}
			if strings.Contains(c, "second question") {
				sawUser2 = true
			}
		case model.RoleAssistant:
			if strings.Contains(c, "first reply") {
				sawAssistant1 = true
			}
		}
	}
	if !sawUser1 || !sawAssistant1 || !sawUser2 {
		t.Fatalf("history not fully replayed:\n  user1=%v assistant1=%v user2=%v\n  messages=%+v",
			sawUser1, sawAssistant1, sawUser2, second)
	}
}

// TestHistoryAccess_None_NoSeedNoAppend asserts an agent declared
// with HistoryAccess=None never sees prior turns AND never writes
// its own. We pre-seed the shared history via a separate ReadWrite
// agent's first call, then issue a None-agent call and confirm:
//   - the None agent's LLM context contains ONLY the new user
//     message (no prior transcript)
//   - the None agent's reply did NOT land in the shared store
//     (a follow-up ReadWrite call must not see "none-reply")
func TestHistoryAccess_None_NoSeedNoAppend(t *testing.T) {
	t.Parallel()
	rwFake := fakellm.New([]fakellm.Step{
		{Text: "rw-first"},
		{Text: "rw-third"},
	}, fakellm.WithRepeatLast())
	noneFake := fakellm.New([]fakellm.Step{
		{Text: "none-reply"},
	}, fakellm.WithRepeatLast())

	vs := spec.Spec{
		ID: "v-hist-none",
		Agents: []spec.Agent{
			{Name: "rw", HistoryAccess: spec.HistoryAccessReadWrite},
			{Name: "isolated", HistoryAccess: spec.HistoryAccessNone},
		},
		History: &spec.History{Kind: "buffer", MaxMessages: 50},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{
			"rw":       rwFake,
			"isolated": noneFake,
		}, 2)),
	)

	ctx := context.Background()
	const cid = "conv-none"
	if _, err := c.Call(ctx, "rw", agent.Request{
		ContextID: cid,
		Message:   model.NewTextMessage(model.RoleUser, "rw-q1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "isolated", agent.Request{
		ContextID: cid,
		Message:   model.NewTextMessage(model.RoleUser, "isolated-q"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "rw", agent.Request{
		ContextID: cid,
		Message:   model.NewTextMessage(model.RoleUser, "rw-q2"),
	}); err != nil {
		t.Fatal(err)
	}

	// Assertion 1: isolated agent's first LLM call saw only the
	// new user message (no rw-q1 / rw-first).
	noneCalls := noneFake.Calls()
	if len(noneCalls) != 1 {
		t.Fatalf("none agent: LLM calls = %d, want 1", len(noneCalls))
	}
	for _, m := range noneCalls[0].Messages {
		c := m.Content()
		if strings.Contains(c, "rw-q1") || strings.Contains(c, "rw-first") {
			t.Fatalf("isolated agent leaked transcript: %q", c)
		}
	}

	// Assertion 2: rw's second LLM call (third overall) saw
	// rw-q1 + rw-first + rw-q2 but NOT isolated-q / none-reply.
	rwCalls := rwFake.Calls()
	if len(rwCalls) != 2 {
		t.Fatalf("rw agent: LLM calls = %d, want 2", len(rwCalls))
	}
	for _, m := range rwCalls[1].Messages {
		c := m.Content()
		if strings.Contains(c, "isolated-q") || strings.Contains(c, "none-reply") {
			t.Fatalf("isolated agent's turn polluted shared transcript: %q", c)
		}
	}
}

// TestHistoryAccess_ReadOnly_SeedsButNoAppend asserts ReadOnly
// agents READ the prior transcript but do NOT write back. We
// seed via a ReadWrite call, run a ReadOnly turn, then run
// another ReadWrite turn and assert the ReadOnly reply is NOT
// in the second ReadWrite agent's seen transcript.
func TestHistoryAccess_ReadOnly_SeedsButNoAppend(t *testing.T) {
	t.Parallel()
	rwFake := fakellm.New([]fakellm.Step{
		{Text: "rw-first"},
		{Text: "rw-second"},
	}, fakellm.WithRepeatLast())
	roFake := fakellm.New([]fakellm.Step{
		{Text: "moderator-only-reply"},
	}, fakellm.WithRepeatLast())

	vs := spec.Spec{
		ID: "v-hist-ro",
		Agents: []spec.Agent{
			{Name: "rw", HistoryAccess: spec.HistoryAccessReadWrite},
			{Name: "moderator", HistoryAccess: spec.HistoryAccessReadOnly},
		},
		History: &spec.History{Kind: "buffer", MaxMessages: 50},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{
			"rw":        rwFake,
			"moderator": roFake,
		}, 2)),
	)

	ctx := context.Background()
	const cid = "conv-ro"
	if _, err := c.Call(ctx, "rw", agent.Request{
		ContextID: cid, Message: model.NewTextMessage(model.RoleUser, "rw-q1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "moderator", agent.Request{
		ContextID: cid, Message: model.NewTextMessage(model.RoleUser, "moderate this"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "rw", agent.Request{
		ContextID: cid, Message: model.NewTextMessage(model.RoleUser, "rw-q2"),
	}); err != nil {
		t.Fatal(err)
	}

	// 1. moderator MUST have seen the rw transcript.
	roCalls := roFake.Calls()
	sawPrior := false
	for _, m := range roCalls[0].Messages {
		if strings.Contains(m.Content(), "rw-q1") || strings.Contains(m.Content(), "rw-first") {
			sawPrior = true
			break
		}
	}
	if !sawPrior {
		t.Fatalf("ReadOnly agent did not receive prior transcript:\n%+v", roCalls[0].Messages)
	}

	// 2. The next rw turn MUST NOT see "moderator-only-reply"
	//    (ReadOnly should not append).
	rwCalls := rwFake.Calls()
	for _, m := range rwCalls[1].Messages {
		if strings.Contains(m.Content(), "moderator-only-reply") || strings.Contains(m.Content(), "moderate this") {
			t.Fatalf("ReadOnly agent's turn polluted history: %q", m.Content())
		}
	}
}

package bindings

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// recordingHost is a hand-built Host stub that captures every call so
// each test assertion can target one method without crosstalk. It is
// intentionally not based on engine.NoopHost: composition would let
// behaviour leak through if a future field on engine.Host were added
// without being mirrored here.
type recordingHost struct {
	publishCalls []event.Envelope
	publishErr   error

	intrCh chan engine.Interrupt

	asks      []engine.UserPrompt
	askReply  engine.UserReply
	askErr    error
	checkpts  []engine.Checkpoint
	checkpErr error
	usages    []model.TokenUsage
}

func newRecordingHost() *recordingHost {
	return &recordingHost{intrCh: make(chan engine.Interrupt, 4)}
}

func (h *recordingHost) Publish(_ context.Context, env event.Envelope) error {
	h.publishCalls = append(h.publishCalls, env)
	return h.publishErr
}
func (h *recordingHost) Interrupts() <-chan engine.Interrupt { return h.intrCh }
func (h *recordingHost) AskUser(_ context.Context, p engine.UserPrompt) (engine.UserReply, error) {
	h.asks = append(h.asks, p)
	return h.askReply, h.askErr
}
func (h *recordingHost) Checkpoint(_ context.Context, cp engine.Checkpoint) error {
	h.checkpts = append(h.checkpts, cp)
	return h.checkpErr
}
func (h *recordingHost) ReportUsage(_ context.Context, u model.TokenUsage) {
	h.usages = append(h.usages, u)
}

// invoke is a tiny convenience that materialises the bridge's API map
// and returns it ready for assertions. The bridge name "host" is also
// returned so tests can verify the canonical global name didn't drift.
func invoke(host engine.Host) (string, map[string]any) {
	name, raw := NewHostBridge(host, "test-source")(context.Background())
	return name, raw.(map[string]any)
}

func TestHostBridge_Name(t *testing.T) {
	name, _ := invoke(engine.NoopHost{})
	if name != "host" {
		t.Fatalf("binding name = %q, want %q", name, "host")
	}
}

func TestHostBridge_Publish_ForwardsEnvelope(t *testing.T) {
	host := newRecordingHost()
	_, api := invoke(host)
	publish := api["publish"].(func(string, any) error)

	if err := publish("agent.test.foo", map[string]any{"hello": "world"}); err != nil {
		t.Fatalf("publish error: %v", err)
	}
	if len(host.publishCalls) != 1 {
		t.Fatalf("publish calls = %d, want 1", len(host.publishCalls))
	}
	if string(host.publishCalls[0].Subject) != "agent.test.foo" {
		t.Fatalf("subject = %q", host.publishCalls[0].Subject)
	}
}

func TestHostBridge_Publish_RejectsBadSubject(t *testing.T) {
	host := newRecordingHost()
	_, api := invoke(host)
	publish := api["publish"].(func(string, any) error)

	if err := publish("", "anything"); err == nil {
		t.Fatal("expected error on empty subject")
	}
	if len(host.publishCalls) != 0 {
		t.Fatalf("host should not see invalid envelope, got %d calls", len(host.publishCalls))
	}
}

func TestHostBridge_CheckInterrupt_NilWhenIdle(t *testing.T) {
	host := newRecordingHost()
	_, api := invoke(host)
	check := api["checkInterrupt"].(func() any)

	if got := check(); got != nil {
		t.Fatalf("check() = %v, want nil on idle host", got)
	}
}

func TestHostBridge_CheckInterrupt_LatchesFirstSignal(t *testing.T) {
	host := newRecordingHost()
	host.intrCh <- engine.Interrupt{Cause: engine.CauseUserCancel, Detail: "stop"}

	_, api := invoke(host)
	check := api["checkInterrupt"].(func() any)

	first := check()
	if first == nil {
		t.Fatal("first check should observe the interrupt")
	}
	m := first.(map[string]any)
	if m["cause"] != string(engine.CauseUserCancel) {
		t.Fatalf("cause = %v, want %q", m["cause"], engine.CauseUserCancel)
	}
	if m["detail"] != "stop" {
		t.Fatalf("detail = %v", m["detail"])
	}

	// Second check must return the same latched value, even though
	// the channel is now drained.
	second := check()
	if second == nil {
		t.Fatal("second check should keep returning the latched interrupt")
	}
	m2 := second.(map[string]any)
	if m2["cause"] != m["cause"] || m2["detail"] != m["detail"] {
		t.Fatalf("latched value drifted: first=%v second=%v", m, m2)
	}
}

func TestHostBridge_AskUser_RoundTripsPartsAndMetadata(t *testing.T) {
	host := newRecordingHost()
	host.askReply = engine.UserReply{
		Parts:    []model.Part{{Type: model.PartText, Text: "ok"}},
		Metadata: map[string]string{"source": "voice"},
	}

	_, api := invoke(host)
	askUser := api["askUser"].(func(any) (map[string]any, error))

	out, err := askUser(map[string]any{
		"parts": []any{
			map[string]any{"type": "text", "text": "Approve?"},
		},
		"schema":   `{"type":"boolean"}`,
		"source":   "approval-step",
		"metadata": map[string]any{"thread": "t1"},
	})
	if err != nil {
		t.Fatalf("askUser error: %v", err)
	}

	if len(host.asks) != 1 {
		t.Fatalf("ask calls = %d, want 1", len(host.asks))
	}
	prompt := host.asks[0]
	if len(prompt.Parts) != 1 || prompt.Parts[0].Text != "Approve?" {
		t.Fatalf("parts = %+v", prompt.Parts)
	}
	if string(prompt.Schema) != `{"type":"boolean"}` {
		t.Fatalf("schema = %q", string(prompt.Schema))
	}
	if prompt.Source != "approval-step" {
		t.Fatalf("source = %q (script override should win)", prompt.Source)
	}
	if prompt.Metadata["thread"] != "t1" {
		t.Fatalf("metadata = %v", prompt.Metadata)
	}

	// Reply projection
	parts, ok := out["parts"].([]map[string]any)
	if !ok || len(parts) != 1 || parts[0]["text"] != "ok" {
		t.Fatalf("reply parts shape = %v", out["parts"])
	}
	meta, ok := out["metadata"].(map[string]any)
	if !ok || meta["source"] != "voice" {
		t.Fatalf("reply metadata = %v", out["metadata"])
	}
}

func TestHostBridge_AskUser_DefaultsSourceFromBridge(t *testing.T) {
	host := newRecordingHost()
	_, api := invoke(host)
	askUser := api["askUser"].(func(any) (map[string]any, error))

	if _, err := askUser(map[string]any{}); err != nil {
		t.Fatalf("askUser error: %v", err)
	}
	if host.asks[0].Source != "test-source" {
		t.Fatalf("source = %q, want bridge default %q",
			host.asks[0].Source, "test-source")
	}
}

func TestHostBridge_AskUser_PropagatesHostError(t *testing.T) {
	host := newRecordingHost()
	host.askErr = errors.New("user closed prompt")
	_, api := invoke(host)
	askUser := api["askUser"].(func(any) (map[string]any, error))

	if _, err := askUser(nil); err == nil || err.Error() != "user closed prompt" {
		t.Fatalf("expected pass-through error, got: %v", err)
	}
}

func TestHostBridge_ReportUsage_DerivesTotalWhenMissing(t *testing.T) {
	host := newRecordingHost()
	_, api := invoke(host)
	report := api["reportUsage"].(func(any) error)

	if err := report(map[string]any{
		"input":  float64(120),
		"output": float64(80),
	}); err != nil {
		t.Fatalf("reportUsage error: %v", err)
	}
	if len(host.usages) != 1 {
		t.Fatalf("usage calls = %d", len(host.usages))
	}
	got := host.usages[0]
	if got.InputTokens != 120 || got.OutputTokens != 80 || got.TotalTokens != 200 {
		t.Fatalf("usage = %+v, want input=120 output=80 total=200", got)
	}
}

func TestHostBridge_ReportUsage_RejectsNonNumber(t *testing.T) {
	host := newRecordingHost()
	_, api := invoke(host)
	report := api["reportUsage"].(func(any) error)

	if err := report(map[string]any{"input": "many"}); err == nil {
		t.Fatal("expected error on non-numeric usage field")
	}
	if len(host.usages) != 0 {
		t.Fatal("usage should not be recorded on parse failure")
	}
}

func TestHostBridge_NilHost_FallsBackToNoop(t *testing.T) {
	// NewHostBridge must accept a nil Host (callers that forgot to
	// install one should still get callable, no-op-ish bindings).
	name, raw := NewHostBridge(nil, "src")(context.Background())
	if name != "host" {
		t.Fatalf("name = %q", name)
	}
	api := raw.(map[string]any)

	check := api["checkInterrupt"].(func() any)
	if got := check(); got != nil {
		t.Fatalf("check() on nil host = %v, want nil", got)
	}

	report := api["reportUsage"].(func(any) error)
	if err := report(map[string]any{"input": float64(1)}); err != nil {
		t.Fatalf("reportUsage on nil host returned error: %v", err)
	}

	publish := api["publish"].(func(string, any) error)
	if err := publish("foo.bar", "x"); err != nil {
		t.Fatalf("publish on nil host returned error: %v", err)
	}
}

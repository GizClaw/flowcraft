package askuser_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	sdkbuiltin "github.com/GizClaw/flowcraft/sdk/tool/builtin/askuser"
	"github.com/GizClaw/flowcraft/sdkx/tool/askuser"
)

// TestForwarder_NameMatchesSDK asserts the sdkx Name constant is
// exactly the value the sdk/tool/builtin/askuser package exports,
// so prompts / registry lookups keyed on either path resolve to
// the same tool. If this drifts, callers migrating their import
// path would silently get a different tool id.
func TestForwarder_NameMatchesSDK(t *testing.T) {
	if askuser.Name != sdkbuiltin.Name {
		t.Fatalf("Name = %q, want %q (sdkx forwarder must re-export the sdk constant verbatim)",
			askuser.Name, sdkbuiltin.Name)
	}
}

// TestForwarder_Definition_Identity confirms the tool produced
// through the sdkx path has the same Name in its Definition as
// the underlying sdk implementation. We do not lock the entire
// schema down here because that is the sdk-side tool's contract;
// duplicating it would just create a second source of truth.
func TestForwarder_Definition_Identity(t *testing.T) {
	def := askuser.New().Definition()
	if def.Name != sdkbuiltin.Name {
		t.Fatalf("Definition.Name = %q, want %q", def.Name, sdkbuiltin.Name)
	}
}

// TestForwarder_Execute_NoHost_NotAvailable proves the forwarder
// does not break the contract documented on the sdk side: an
// ask_user call on a ctx that does not carry an engine.Host must
// surface errdefs.NotAvailable. This is the smoke test that a
// no-op forwarder change would still trip if someone accidentally
// reimplemented New() to return a different impl.
func TestForwarder_Execute_NoHost_NotAvailable(t *testing.T) {
	_, err := askuser.New().Execute(context.Background(), `{"prompt":"hi"}`)
	if err == nil {
		t.Fatal("expected error on bare ctx (no engine.Host)")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err category = %v, want NotAvailable", err)
	}
}

// TestForwarder_Execute_HappyPath threads a fake host through
// engine.WithHost and confirms the sdkx-constructed tool reaches
// it. This is the single end-to-end check that the forwarder
// pipes the host-on-ctx mechanism through unchanged; substantive
// host-side behaviour is covered in sdk/tool/builtin/askuser.
func TestForwarder_Execute_HappyPath(t *testing.T) {
	host := &replyingHost{reply: "you should ship"}
	tl := askuser.New()

	ctx := engine.WithHost(context.Background(), host)
	out, err := tl.Execute(ctx, `{"prompt":"should I ship?"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "you should ship") {
		t.Fatalf("result = %q, want host reply substring", out)
	}
	if host.gotPrompt.Source != askuser.Name {
		t.Errorf("UserPrompt.Source = %q, want %q (Execute must stamp the source)",
			host.gotPrompt.Source, askuser.Name)
	}
}

// replyingHost embeds NoopHost so it satisfies the full
// engine.Host surface, then overrides AskUser to return a
// scripted reply and capture the inbound prompt for assertion.
type replyingHost struct {
	engine.NoopHost
	gotPrompt engine.UserPrompt
	reply     string
}

func (h *replyingHost) AskUser(_ context.Context, p engine.UserPrompt) (engine.UserReply, error) {
	h.gotPrompt = p
	return engine.UserReply{
		Parts: []model.Part{{Type: model.PartText, Text: h.reply}},
	}, nil
}

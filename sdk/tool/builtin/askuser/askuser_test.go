package askuser_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/builtin/askuser"
	"github.com/GizClaw/flowcraft/sdk/tool/tooltest"
)

// TestAskUser_Contract pins the askuser tool against the generic
// tool.Tool contract suite. ask_user has a slightly unusual schema
// — it requires a non-empty `prompt` property — so we declare
// SkipEmptyArgsTolerance: the suite would otherwise complain that
// empty args raised a Validation error.
//
// SkipContextCancel is left false (= the suite will check
// cancellation responsiveness). ask_user's Execute returns
// promptly when there's no host on ctx, so it satisfies the
// "return within the deadline of a pre-cancelled ctx" check.
func TestAskUser_Contract(t *testing.T) {
	tooltest.RunSuite(t, func() tool.Tool { return askuser.New() }, tooltest.Capabilities{
		SkipEmptyArgsTolerance: true,
	})
}

// captureHost records the prompt and returns a programmable reply.
// Used by tests to assert prompt translation + reply marshalling.
type captureHost struct {
	engine.NoopHost
	gotPrompt engine.UserPrompt
	reply     engine.UserReply
	err       error
}

func (h *captureHost) AskUser(_ context.Context, prompt engine.UserPrompt) (engine.UserReply, error) {
	h.gotPrompt = prompt
	return h.reply, h.err
}

func TestAskUser_HappyPath(t *testing.T) {
	host := &captureHost{
		reply: engine.UserReply{Parts: []model.Part{{Type: model.PartText, Text: "yes please"}}},
	}
	ctx := engine.WithHost(context.Background(), host)
	out, err := askuser.New().Execute(ctx, `{"prompt":"shall I proceed?"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "yes please" {
		t.Errorf("reply = %q, want %q", out, "yes please")
	}
	if got := host.gotPrompt.Parts[0].Text; got != "shall I proceed?" {
		t.Errorf("forwarded prompt = %q, want original", got)
	}
	if host.gotPrompt.Source != askuser.Name {
		t.Errorf("prompt.Source = %q, want %q", host.gotPrompt.Source, askuser.Name)
	}
}

func TestAskUser_NoHostInCtxIsNotAvailable(t *testing.T) {
	_, err := askuser.New().Execute(context.Background(), `{"prompt":"hi"}`)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("missing host: want NotAvailable, got %v", err)
	}
}

func TestAskUser_EmptyPromptIsValidation(t *testing.T) {
	ctx := engine.WithHost(context.Background(), &captureHost{})
	for _, p := range []string{`{"prompt":""}`, `{"prompt":"   "}`, `{}`} {
		_, err := askuser.New().Execute(ctx, p)
		if !errdefs.IsValidation(err) {
			t.Errorf("payload %q: want Validation, got %v", p, err)
		}
	}
}

func TestAskUser_BadJSONIsValidation(t *testing.T) {
	ctx := engine.WithHost(context.Background(), &captureHost{})
	_, err := askuser.New().Execute(ctx, `{not-json`)
	if !errdefs.IsValidation(err) {
		t.Fatalf("bad json: want Validation, got %v", err)
	}
}

func TestAskUser_HostErrorPropagates(t *testing.T) {
	wantErr := errors.New("user declined")
	host := &captureHost{err: wantErr}
	ctx := engine.WithHost(context.Background(), host)
	_, err := askuser.New().Execute(ctx, `{"prompt":"go?"}`)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

func TestAskUser_NonTextPartsRenderAsMarker(t *testing.T) {
	host := &captureHost{
		reply: engine.UserReply{Parts: []model.Part{
			{Type: model.PartText, Text: "see attached"},
			{Type: model.PartImage},
		}},
	}
	ctx := engine.WithHost(context.Background(), host)
	out, err := askuser.New().Execute(ctx, `{"prompt":"q"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "see attached") {
		t.Errorf("text part missing in %q", out)
	}
	if !strings.Contains(out, "image") {
		t.Errorf("image marker missing in %q", out)
	}
}

func TestAskUser_DefinitionStable(t *testing.T) {
	def := askuser.New().Definition()
	if def.Name != askuser.Name {
		t.Errorf("Definition.Name = %q, want %q", def.Name, askuser.Name)
	}
	if def.Description == "" {
		t.Error("Definition.Description is empty; LLM has no usage hint")
	}
	props, _ := def.InputSchema["properties"].(map[string]any)
	if _, ok := props["prompt"]; !ok {
		t.Error("schema missing required 'prompt' property")
	}
}

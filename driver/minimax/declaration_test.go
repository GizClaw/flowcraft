package minimax

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestDeclaredLineUpPublishesItsFacts locks the declaration contract: the
// provider publishes exactly the models a deployment declares, each carrying
// the capabilities and limits it stated, and the published order does not
// depend on the config's order.
func TestDeclaredLineUpPublishesItsFacts(t *testing.T) {
	settings := ResourceSettings{
		ID: "minimax",
		Spec: json.RawMessage(`{"models":[
			{"name":"MiniMax-H3","kind":"video",
			 "capabilities":{"inputs":["text","image"],"outputs":["video"]},
			 "video":{"api":"v2"},
			 "lifecycle":{"status":"deprecated","replacement":
			   {"provider":"minimax","name":"speech-2.8-hd"}}},
			{"name":"speech-2.8-hd","kind":"tts",
			 "capabilities":{"inputs":["text"],"outputs":["audio"]},
			 "limits":{"max_input_tokens":1000}}
		]}`),
	}
	provider, err := buildProvider(context.Background(), settings, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	var names []string
	byName := make(map[string]model.ModelDescriptor, len(provider.Models))
	for _, impl := range provider.Models {
		names = append(names, impl.Descriptor.ID.Name)
		byName[impl.Descriptor.ID.Name] = impl.Descriptor
	}
	if len(names) != 2 || names[0] != "MiniMax-H3" || names[1] != "speech-2.8-hd" {
		t.Fatalf("published models = %v", names)
	}
	h3 := byName["MiniMax-H3"]
	if len(h3.Capabilities.Outputs) != 1 ||
		h3.Capabilities.Outputs[0] != message.PartVideo {
		t.Fatalf("h3 outputs = %v, want video", h3.Capabilities.Outputs)
	}
	if h3.Lifecycle.Status != model.ModelStatusDeprecated ||
		h3.Lifecycle.Replacement == nil ||
		h3.Lifecycle.Replacement.Name != "speech-2.8-hd" {
		t.Fatalf("h3 lifecycle = %+v", h3.Lifecycle)
	}
	if in, out := byName["speech-2.8-hd"].Limits.Values(); in != 1000 || out != 0 {
		t.Fatalf("tts limits = %d/%d, want 1000/undeclared", in, out)
	}
}

// TestFamilyContractRejections locks the boundary between a declaration and
// the compiler bound by kind: a model whose declared facts the family cannot
// serve fails at build time instead of failing on every request.
func TestFamilyContractRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "image without image output",
			raw:  `{"name":"m","kind":"image","capabilities":{"outputs":["text"]}}`,
			want: "image output",
		},
		{
			name: "tts without audio output",
			raw:  `{"name":"m","kind":"tts","capabilities":{"outputs":["text"]}}`,
			want: "audio output",
		},
		{
			name: "video without video output",
			raw:  `{"name":"m","kind":"video","capabilities":{"outputs":["text"]}}`,
			want: "video output",
		},
		{
			name: "context_ir without text output",
			raw:  `{"name":"m","kind":"context_ir","capabilities":{"outputs":["video"]}}`,
			want: "text output",
		},
		{
			name: "text generation moved to the anthropic driver",
			raw:  `{"name":"m","kind":"generate"}`,
			want: "anthropic driver",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := decodeSpec(context.Background(), []byte(
				`{"models":[`+tc.raw+`]}`,
			))
			if err != nil && strings.Contains(err.Error(), tc.want) {
				// Some contract violations are structural enough to fail
				// strict decoding already, which is an even earlier gate.
				return
			}
			if err != nil {
				t.Fatalf("decodeSpec: %v", err)
			}
			_, err = resolveModelsForTest(t, spec)
			if err == nil {
				t.Fatal("resolveModels accepted a contract violation")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestVideoSurfaceDeclaration locks the video vocabulary: a video model says
// which task API it speaks and which tiers it accepts, an unknown API is
// rejected, and the control facts stay off the other kinds.
func TestVideoSurfaceDeclaration(t *testing.T) {
	spec := decodeSpecWithModels(t, "", "MiniMax-H3", "MiniMax-Hailuo-02")
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	h3 := models["MiniMax-H3"].spec
	if !h3.videoV2() {
		t.Fatalf("MiniMax-H3 video = %+v, want the v2 task API", h3.Video)
	}
	hailuo := models["MiniMax-Hailuo-02"].spec
	if hailuo.videoV2() || !hailuo.Video.LastFrame || !hailuo.Video.P512 {
		t.Fatalf("MiniMax-Hailuo-02 video = %+v", hailuo.Video)
	}

	for _, raw := range []string{
		`{"models":[{"name":"m","kind":"video","capabilities":{"outputs":["video"]},` +
			`"video":{"api":"v3"}}]}`,
		`{"models":[{"name":"m","kind":"tts","capabilities":{"outputs":["audio"]},` +
			`"video":{"hd":true}}]}`,
	} {
		if _, err := decodeSpec(context.Background(), []byte(raw)); err == nil {
			t.Fatalf("decodeSpec(%s) accepted an invalid video declaration", raw)
		}
	}
}

// TestWireModelAliasRoutesToTarget locks the alias leaf: a declaration can
// serve an operation under one name while addressing another model on the
// wire.
func TestWireModelAliasRoutesToTarget(t *testing.T) {
	spec := decodeSpecWithModels(t, "", "MiniMax-H3-Context-IR")
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	alias := models["MiniMax-H3-Context-IR"].spec
	if got := wireModel("MiniMax-H3-Context-IR", alias); got != "MiniMax-H3" {
		t.Fatalf("wire model = %q, want MiniMax-H3", got)
	}
	if len(alias.Capabilities.Inputs) == 0 ||
		alias.Capabilities.Inputs[0] != message.PartText {
		t.Fatalf("alias inputs = %v", alias.Capabilities.Inputs)
	}
}

// TestFixtureSetIsWellFormed keeps the fixture declarations themselves on the
// family contract, so a fixture cannot drift into a shape the provider would
// reject.
func TestFixtureSetIsWellFormed(t *testing.T) {
	spec := decodeSpecWithModels(t, "", fixtureNames...)
	if _, err := resolveModelsForTest(t, spec); err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
}

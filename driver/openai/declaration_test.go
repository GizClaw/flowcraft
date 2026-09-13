package openai

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestDeclaredLineUpPublishesItsFacts locks the declaration contract: the
// provider publishes exactly the models a deployment declares, each carrying
// the capabilities and limits it stated. Nothing is inherited from a driver
// line-up, and the published order does not depend on the config's order.
func TestDeclaredLineUpPublishesItsFacts(t *testing.T) {
	settings := ResourceSettings{
		ID: "gateway",
		Spec: json.RawMessage(`{"models":[
			{"name":"zeta","kind":"generate",
			 "capabilities":{"inputs":["text","image"],"outputs":["text"],
			   "hosted_web_search":true,
			   "reasoning":{"kind":"toggle","effort_map":{
			     "minimal":"minimal","low":"low","medium":"medium",
			     "high":"high","xhigh":"high"}}},
			 "limits":{"max_input_tokens":1000,"max_output_tokens":100}},
			{"name":"alpha","kind":"image",
			 "capabilities":{"inputs":["text"],"outputs":["image"]}},
			{"name":"beta","kind":"tts","capabilities":{"outputs":["audio"]}},
			{"name":"gamma","kind":"embed",
			 "capabilities":{"custom_embed_dimensions":true}}
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
	if want := []string{"alpha", "beta", "gamma", "zeta"}; !slices.Equal(names, want) {
		t.Fatalf("published models = %v, want %v", names, want)
	}

	zeta := byName["zeta"].Capabilities
	if !slices.Contains(zeta.Inputs, message.PartImage) ||
		!slices.Equal(zeta.Outputs, []message.PartKind{message.PartText}) {
		t.Fatalf("zeta capabilities = %+v", zeta)
	}
	if !zeta.HostedWebSearch ||
		zeta.Reasoning.Kind != model.ReasoningToggle ||
		len(zeta.Reasoning.EffortMap) != 5 {
		t.Fatalf("zeta capabilities = %+v", zeta)
	}
	if in, out := byName["zeta"].Limits.Values(); in != 1000 || out != 100 {
		t.Fatalf("zeta limits = %d/%d, want 1000/100", in, out)
	}

	if outputs := byName["alpha"].Capabilities.Outputs; !slices.Equal(
		outputs, []message.PartKind{message.PartImage},
	) {
		t.Fatalf("alpha outputs = %v, want image", outputs)
	}
	if outputs := byName["beta"].Capabilities.Outputs; !slices.Equal(
		outputs, []message.PartKind{message.PartAudio},
	) {
		t.Fatalf("beta outputs = %v, want audio", outputs)
	}
	gamma := byName["gamma"].Capabilities
	if len(gamma.Outputs) != 0 || !gamma.CustomEmbedDimensions {
		t.Fatalf("gamma capabilities = %+v", gamma)
	}
	if in, out := byName["gamma"].Limits.Values(); in != 0 || out != 0 {
		t.Fatalf("undeclared limits = %d/%d, want them undeclared", in, out)
	}
}

// TestFamilyContractRejections locks the boundary between the declaration and
// the compiler bound by kind: a model whose declared facts the family cannot
// serve fails at build time instead of failing on every request.
func TestFamilyContractRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "generate without text output",
			raw:  `{"name":"m","kind":"generate"}`,
			want: "text output",
		},
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
			name: "embed with generate output",
			raw:  `{"name":"m","kind":"embed","capabilities":{"outputs":["text"]}}`,
			want: "declares no generate output",
		},
		{
			name: "audio input",
			raw: `{"name":"m","kind":"generate",` +
				`"capabilities":{"inputs":["audio"],"outputs":["text"]}}`,
			want: "no audio input",
		},
		{
			name: "file input",
			raw: `{"name":"m","kind":"generate",` +
				`"capabilities":{"inputs":["file"],"outputs":["text"]}}`,
			want: "no file input",
		},
		{
			name: "video input without the endpoint fact",
			raw: `{"name":"m","kind":"generate",` +
				`"capabilities":{"inputs":["video"],"outputs":["text"]}}`,
			want: "spec.wire.video_input",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := ResourceSettings{
				ID: "gateway",
				Spec: json.RawMessage(
					`{"models":[` + tc.raw + `]}`,
				),
			}
			_, err := buildProvider(context.Background(), settings, nil)
			if err == nil {
				t.Fatal("buildProvider accepted a contract violation")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestCustomEmbedDimensionsRequiresEmbedKind locks the one capability leaf
// whose meaning depends on the family: only an embed model may promise the
// optional output-dimensions parameter.
func TestCustomEmbedDimensionsRequiresEmbedKind(t *testing.T) {
	settings := ResourceSettings{
		ID: "gateway",
		Spec: json.RawMessage(`{"models":[{
			"name":"m","kind":"generate",
			"capabilities":{"outputs":["text"],"custom_embed_dimensions":true}
		}]}`),
	}
	if _, err := buildProvider(context.Background(), settings, nil); err == nil {
		t.Fatal("generate model accepted custom_embed_dimensions")
	}
}

// TestDeclaredLifecycleReachesTheDescriptor locks the lifecycle leaf: a
// deployment marks its own models deprecated and names the replacement, and
// the published descriptor carries the same facts under the deployment's own
// provider identity.
func TestDeclaredLifecycleReachesTheDescriptor(t *testing.T) {
	settings := ResourceSettings{
		ID: "gateway",
		Spec: json.RawMessage(`{"models":[
			{"name":"old","kind":"generate","capabilities":{"outputs":["text"]},
			 "lifecycle":{"status":"deprecated","notes":"moved to a cheaper tier",
			   "replacement":{"provider":"gateway","name":"new"}}},
			{"name":"new","kind":"generate","capabilities":{"outputs":["text"]}}
		]}`),
	}
	provider, err := buildProvider(context.Background(), settings, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]model.ModelDescriptor, len(provider.Models))
	for _, impl := range provider.Models {
		descriptors[impl.Descriptor.ID.Name] = impl.Descriptor
	}
	lifecycle := descriptors["old"].Lifecycle
	if lifecycle.Status != model.ModelStatusDeprecated ||
		lifecycle.Notes != "moved to a cheaper tier" {
		t.Fatalf("lifecycle = %+v", lifecycle)
	}
	if lifecycle.Replacement == nil ||
		*lifecycle.Replacement != (model.ModelID{Provider: "gateway", Name: "new"}) {
		t.Fatalf("replacement = %+v", lifecycle.Replacement)
	}
	// An undeclared lifecycle stays empty: the zero value means active, and
	// the driver does not invent a status the deployment never wrote.
	if undeclared := descriptors["new"].Lifecycle; undeclared != (model.ModelLifecycle{}) {
		t.Fatalf("undeclared lifecycle = %+v, want empty", undeclared)
	}
}

// TestLifecycleRejections locks the boundary: lifecycle facts are checked
// against the model's own identity, so a model cannot be replaced by itself
// and an active model cannot carry retirement metadata.
func TestLifecycleRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "self replacement",
			body: `"lifecycle":{"status":"deprecated",` +
				`"replacement":{"provider":"gateway","name":"m"}}`,
		},
		{
			name: "active with replacement",
			body: `"lifecycle":{"status":"active",` +
				`"replacement":{"provider":"gateway","name":"other"}}`,
		},
		{name: "unknown status", body: `"lifecycle":{"status":"sunset"}`},
		{name: "replacement without provider",
			body: `"lifecycle":{"status":"deprecated","replacement":{"name":"other"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := ResourceSettings{
				ID: "gateway",
				Spec: json.RawMessage(`{"models":[{"name":"m","kind":"generate",` +
					`"capabilities":{"outputs":["text"]},` + tc.body + `}]}`),
			}
			if _, err := buildProvider(context.Background(), settings, nil); err == nil {
				t.Fatal("buildProvider accepted an invalid lifecycle")
			}
		})
	}
}

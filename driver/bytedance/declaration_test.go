package bytedance

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
// the capabilities, limits, and lifecycle it stated, under the deployment's
// own provider identity and in a stable order.
func TestDeclaredLineUpPublishesItsFacts(t *testing.T) {
	settings := ResourceSettings{
		ID: "ark",
		Spec: json.RawMessage(`{"models":[
			{"name":"pro","kind":"generate",
			 "capabilities":{"inputs":["text","image"],"outputs":["text"]},
			 "limits":{"max_input_tokens":256000,"max_output_tokens":256000},
			 "lifecycle":{"status":"deprecated","replacement":
			   {"provider":"ark","name":"lite"}}},
			{"name":"lite","kind":"generate",
			 "capabilities":{"inputs":["text"],"outputs":["text"]}}
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
	if len(names) != 2 || names[0] != "lite" || names[1] != "pro" {
		t.Fatalf("published models = %v, want them sorted", names)
	}
	pro := byName["pro"]
	if len(pro.Capabilities.Outputs) != 1 ||
		pro.Capabilities.Outputs[0] != message.PartText {
		t.Fatalf("pro outputs = %v, want text", pro.Capabilities.Outputs)
	}
	if in, out := pro.Limits.Values(); in != 256_000 || out != 256_000 {
		t.Fatalf("pro limits = %d/%d", in, out)
	}
	if pro.Lifecycle.Status != model.ModelStatusDeprecated ||
		pro.Lifecycle.Replacement == nil ||
		pro.Lifecycle.Replacement.Name != "lite" {
		t.Fatalf("pro lifecycle = %+v", pro.Lifecycle)
	}
	if active := byName["lite"].Lifecycle; active != (model.ModelLifecycle{}) {
		t.Fatalf("undeclared lifecycle = %+v, want empty", active)
	}
}

// TestFamilyContractRejections locks the boundary between a declaration and
// the compiler bound by kind.
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
			name: "video without video output",
			raw:  `{"name":"m","kind":"video","capabilities":{"outputs":["text"]}}`,
			want: "video output",
		},
		{
			name: "embed with generate output",
			raw:  `{"name":"m","kind":"embed","capabilities":{"outputs":["text"]}}`,
			want: "declares no generate output",
		},
		{
			name: "reference images without image input",
			raw: `{"name":"m","kind":"video",` +
				`"capabilities":{"inputs":["text"],"outputs":["video"]},` +
				`"video":{"reference_image":2}}`,
			want: "must accept image input",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := decodeSpec(context.Background(), []byte(
				`{"models":[`+tc.raw+`]}`,
			))
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

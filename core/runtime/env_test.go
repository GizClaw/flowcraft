package runtime

import (
	"context"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func envLayer() Layer {
	return Layer{Priority: 0, Name: "forge.yaml", Bytes: []byte(`
sandbox:
  backend: local
files:
  roots: ["${ROOT}"]
inference:
  models:
    - name: primary
      model: m
      driver: d
      settings:
        token: "${TOKEN}"
        region: "${REGION:-us-east-1}"
# a commented reference is not a reference: ${IGNORED}
`)}
}

// A host can describe what it needs before anything is set, instead of failing
// one variable per restart.
func TestRequiredEnvClassifiesReferences(t *testing.T) {
	requirement, err := RequiredEnv(envLayer())
	if err != nil {
		t.Fatalf("RequiredEnv: %v", err)
	}
	if got := strings.Join(requirement.Required, ","); got != "ROOT,TOKEN" {
		t.Errorf("required = %q, want ROOT,TOKEN", got)
	}
	if got := strings.Join(requirement.Optional, ","); got != "REGION" {
		t.Errorf("optional = %q, want REGION", got)
	}
	for _, unexpected := range []string{"IGNORED"} {
		for _, name := range append(requirement.Required, requirement.Optional...) {
			if name == unexpected {
				t.Errorf("%s was collected from a comment", unexpected)
			}
		}
	}
	if got := requirement.Sources["ROOT"]; len(got) != 1 || got[0] != "forge.yaml" {
		t.Errorf("sources = %v, want forge.yaml", got)
	}
}

func TestEnvRequirementMissing(t *testing.T) {
	requirement, err := RequiredEnv(envLayer())
	if err != nil {
		t.Fatalf("RequiredEnv: %v", err)
	}
	env := map[string]string{"ROOT": "/work"}
	missing := requirement.Missing(func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	})
	if len(missing) != 1 || missing[0] != "TOKEN" {
		t.Errorf("missing = %v, want TOKEN only", missing)
	}
	if got := requirement.Missing(nil); got != nil {
		t.Errorf("Missing(nil) = %v, want nil", got)
	}
}

// "$$" is a literal dollar, not a reference, while a real reference on the same
// line still counts. The scanner is shared with expansion, so both views must
// agree about which is which.
func TestRequiredEnvAndExpansionAgree(t *testing.T) {
	layer := Layer{Name: "a.yaml", Bytes: []byte(`
sandbox: {backend: local}
files: {roots: ["/work"]}
name: "cost is $$5, not ${MONEY}"
`)}
	requirement, err := RequiredEnv(layer)
	if err != nil {
		t.Fatalf("RequiredEnv: %v", err)
	}
	if got := strings.Join(requirement.Required, ","); got != "MONEY" {
		t.Errorf("required = %q, want MONEY only", got)
	}

	t.Setenv("MONEY", "free")
	body, err := layer.body()
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := resolvePlaceholders(context.Background(), documentBody(&document), "", loadOptions{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	encoded, err := yaml.Marshal(&document)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), "cost is $5, not free") {
		t.Errorf("expanded = %q, want the literal dollar preserved", encoded)
	}
}

// One document, no layers: the deployment loader needs the answer before it can
// build the layer stack, and it needs to name the file to edit.
func TestRequiredEnvDocument(t *testing.T) {
	requirement, err := RequiredEnvDocument([]byte(`
name: ${NAME}
sandbox:
  memory_mib: ${MEM_MIB:-512}
`), "deploy.yaml")
	if err != nil {
		t.Fatalf("RequiredEnvDocument: %v", err)
	}
	if len(requirement.Required) != 1 || requirement.Required[0] != "NAME" {
		t.Errorf("required = %v, want [NAME]", requirement.Required)
	}
	if len(requirement.Optional) != 1 || requirement.Optional[0] != "MEM_MIB" {
		t.Errorf("optional = %v, want [MEM_MIB]", requirement.Optional)
	}
	if sources := requirement.Sources["NAME"]; len(sources) != 1 || sources[0] != "deploy.yaml" {
		t.Errorf("sources = %v, want [deploy.yaml]", sources)
	}
}

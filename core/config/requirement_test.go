package config

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"go.yaml.in/yaml/v3"
)

func envLayer() Layer {
	return Layer{Priority: 0, Name: "forge.yaml", Source: Bytes([]byte(`
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
`))}
}

// A host can describe what it needs before anything is set, instead of failing
// one variable per restart.
func TestRequiredPlaceholdersClassifiesReferences(t *testing.T) {
	requirement, err := RequiredPlaceholders(context.Background(), envLayer())
	if err != nil {
		t.Fatalf("RequiredPlaceholders: %v", err)
	}
	if got := strings.Join(requirement.Required, ","); got != "ROOT,TOKEN" {
		t.Errorf("required = %q, want ROOT,TOKEN", got)
	}
	if got := strings.Join(requirement.Optional, ","); got != "REGION" {
		t.Errorf("optional = %q, want REGION", got)
	}
	for _, name := range append(requirement.Required, requirement.Optional...) {
		if name == "IGNORED" {
			t.Errorf("%s was collected from a comment", name)
		}
	}
	if got := requirement.Sources["ROOT"]; len(got) != 1 || got[0] != "forge.yaml" {
		t.Errorf("sources = %v, want forge.yaml", got)
	}
}

func TestRequirementMissing(t *testing.T) {
	requirement, err := RequiredPlaceholders(context.Background(), envLayer())
	if err != nil {
		t.Fatalf("RequiredPlaceholders: %v", err)
	}
	env := map[string]string{"ROOT": pathtest.Abs("work")}
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

// The same variable in two layers is reported once, under both names, so the
// reader is sent to every file that has to change.
func TestRequiredPlaceholdersNamesEverySource(t *testing.T) {
	requirement, err := RequiredPlaceholders(context.Background(),
		Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte("name: ${TOKEN}\n"))},
		Layer{Priority: 10, Name: "local.yaml", Source: Bytes([]byte("other: ${TOKEN}\n"))},
	)
	if err != nil {
		t.Fatalf("RequiredPlaceholders: %v", err)
	}
	if got := strings.Join(requirement.Sources["TOKEN"], ","); got != "base.yaml,local.yaml" {
		t.Errorf("sources = %q", got)
	}
	if len(requirement.Required) != 1 {
		t.Errorf("required = %v, want TOKEN once", requirement.Required)
	}
}

// "$$" is a literal dollar, not a reference, while a real reference on the same
// line still counts. The scanner is shared with expansion, so both views must
// agree about which is which.
func TestRequiredPlaceholdersAndExpansionAgree(t *testing.T) {
	layer := Layer{Name: "a.yaml", Source: Bytes([]byte(`
name: "cost is $$5, not ${MONEY}"
`))}
	requirement, err := RequiredPlaceholders(context.Background(), layer)
	if err != nil {
		t.Fatalf("RequiredPlaceholders: %v", err)
	}
	if got := strings.Join(requirement.Required, ","); got != "MONEY" {
		t.Errorf("required = %q, want MONEY only", got)
	}

	t.Setenv("MONEY", "free")
	body, err := layer.body(context.Background(), defaultLabel)
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := resolvePlaceholders(context.Background(), documentBody(&document), "", newOptions(nil)); err != nil {
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

// One document, no layers: a host that owns the whole file needs the answer
// before it can build a layer stack, and it needs the source named so the error
// points at the file to edit.
func TestRequiredPlaceholdersDocument(t *testing.T) {
	requirement, err := RequiredPlaceholdersDocument([]byte(`
name: ${NAME}
sandbox:
  memory_mib: ${MEM_MIB:-512}
`), "deploy.yaml")
	if err != nil {
		t.Fatalf("RequiredPlaceholdersDocument: %v", err)
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

// A malformed reference is reported against the file it is in: the name is what
// the reader is told to edit, and a checklist that cannot say where the mistake
// is has answered half the question.
func TestRequiredPlaceholdersNamesTheDocumentOnAMalformedReference(t *testing.T) {
	layer := Layer{Name: "forge.yaml", Source: Bytes([]byte(`
sandbox:
  backend: ${BROKEN
`))}
	_, err := RequiredPlaceholders(context.Background(), layer)
	if err == nil || !strings.Contains(err.Error(), "forge.yaml: unterminated ${") {
		t.Errorf("layer err = %v, want the layer's name", err)
	}
	_, err = RequiredPlaceholdersDocument([]byte("name: ${BROKEN\n"), "deploy.yaml")
	if err == nil || !strings.Contains(err.Error(), "deploy.yaml: unterminated ${") {
		t.Errorf("document err = %v, want the document's name", err)
	}
}

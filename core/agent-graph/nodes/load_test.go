// Loading a document: what the loader expands, and what it refuses.
package nodes_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/config"
)

// A graph document is expanded the way a runtime layer is, so a node config may
// name a scheme the host registered. The node kind is not checked here - Validate
// reads the document, it does not build a registry - which is why this document
// can name a kind no host defines.
const schemeGraph = `version: 1
name: g
entry: step
state:
  k: {type: text}
nodes:
  - {id: step, kind: append, config: {key: k, text: "${demo:greeting}"}}
edges:
  - {from: step, to: end}
`

func TestGraphDocumentUsesARegisteredScheme(t *testing.T) {
	resolve := func(_ context.Context, args []string) (string, error) {
		if len(args) != 1 || args[0] != "greeting" {
			t.Errorf("resolver saw %v, want [greeting]", args)
		}
		return "hi", nil
	}
	err := nodes.Validate(context.Background(), "g", config.Bytes([]byte(schemeGraph)),
		nodes.WithConfigOptions(config.WithResolver("demo", resolve)))
	if err != nil {
		t.Fatalf("a registered scheme was refused: %v", err)
	}
}

// Without the option the load fails, and the error names what is registered: a
// host that forgot to pass its resolvers reads which scheme is unknown and what
// it could have named.
func TestGraphDocumentRefusesAnUnregisteredScheme(t *testing.T) {
	err := nodes.Validate(context.Background(), "g", config.Bytes([]byte(schemeGraph)))
	if err == nil {
		t.Fatal("an unregistered scheme was accepted")
	}
	for _, want := range []string{`unknown placeholder scheme "demo"`, "offered: env", "text", "g:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// A load that cannot expand the document names every reference it makes, so a
// host's startup message is the whole checklist - not one missing value per
// restart. This is where the report above is used outside a host's own call.
func TestGraphDocumentFailureListsEveryReference(t *testing.T) {
	document := strings.ReplaceAll(schemeGraph, "${demo:greeting}",
		"${GRAPH_FIRST} ${GRAPH_SECOND:-fallback} ${demo:greeting}")
	err := nodes.Validate(context.Background(), "g", config.Bytes([]byte(document)))
	if err == nil {
		t.Fatal("a document naming unset references was accepted")
	}
	for _, want := range []string{
		"${GRAPH_FIRST} is not set",
		"the document references: ",
		"GRAPH_FIRST",
		"demo:greeting",
		"GRAPH_SECOND (optional)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// The built-in scheme keeps working with no options at all: the option is
// additive, not a new requirement.
func TestGraphDocumentExpandsEnvWithoutOptions(t *testing.T) {
	t.Setenv("GRAPH_LOAD_TEST_TEXT", "from-the-environment")
	document := strings.ReplaceAll(schemeGraph, "${demo:greeting}", "${GRAPH_LOAD_TEST_TEXT}")
	if err := nodes.Validate(context.Background(), "g", config.Bytes([]byte(document))); err != nil {
		t.Fatalf("env placeholder: %v", err)
	}
}

// A checklist is reported over the same scanner expansion uses, so a host can
// name every reference once instead of restarting once per missing value.
func TestGraphRequiredPlaceholdersReportsEveryReference(t *testing.T) {
	document := strings.ReplaceAll(schemeGraph, "${demo:greeting}", "${GRAPH_MISSING} ${demo:greeting}")
	requirement, err := nodes.RequiredPlaceholders(
		context.Background(), "g", config.Bytes([]byte(document)))
	if err != nil {
		t.Fatalf("RequiredPlaceholders: %v", err)
	}
	if got, want := strings.Join(requirement.Required, ","), "GRAPH_MISSING,demo:greeting"; got != want {
		t.Errorf("required = %q, want %q", got, want)
	}
	if missing := requirement.Missing(func(string) (string, bool) { return "", false }); len(missing) != 2 {
		t.Errorf("missing = %v, want both references", missing)
	}
}

// A fallback is optional, which is what keeps ${NAME:-default} out of the
// "cannot start" list.
func TestGraphRequiredPlaceholdersSeparatesFallbacks(t *testing.T) {
	document := strings.ReplaceAll(schemeGraph, "${demo:greeting}", "${GRAPH_OPTIONAL:-fallback}")
	requirement, err := nodes.RequiredPlaceholders(
		context.Background(), "g", config.Bytes([]byte(document)))
	if err != nil {
		t.Fatalf("RequiredPlaceholders: %v", err)
	}
	if len(requirement.Required) != 0 || len(requirement.Optional) != 1 {
		t.Errorf("required = %v, optional = %v; want only the fallback",
			requirement.Required, requirement.Optional)
	}
}

// The label is what a reader uses to find the value, and a host may replace it.
func TestGraphDocumentLabelIsOverridable(t *testing.T) {
	err := nodes.Validate(context.Background(), "g", config.Bytes([]byte(schemeGraph)),
		nodes.WithConfigOptions(config.WithLabel("inner-label")))
	if err == nil || !strings.Contains(err.Error(), "inner-label: nodes.config.text") {
		t.Errorf("error = %v, want the host's label and the path", err)
	}
}

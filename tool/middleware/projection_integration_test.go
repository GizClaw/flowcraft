package middleware_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/projection"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
	"github.com/GizClaw/flowcraft/tool/dispatch"
)

// TestADispatchedCallFeedsTheProjection is the join between the two packages that only
// existed on paper until now: dispatch runs a call, reports it to the session's projection,
// and the tool the model used is visible in the next round.
func TestADispatchedCallFeedsTheProjection(t *testing.T) {
	used := tool.TextTool(message.ToolDefinition{
		Name:        "used",
		Description: "a tool the model called",
		InputSchema: []byte(`{"type":"object"}`),
	}, func(context.Context, string) (string, error) { return "ok", nil })

	catalog, err := tool.NewRegistry([]tool.Source{tooltest.Source(used)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	view := mustProjection(t, catalog).NewView()

	// Deferred by default: nothing is visible before the call.
	if view.Visible("used") {
		t.Fatal("the tool was visible before it was used")
	}

	executor := dispatch.New(catalog)
	binding := tool.Bind(tooltest.Session("s1"), policy.Grant{}).WithView(view)
	result := executor.Execute(context.Background(), binding, message.ToolCall{
		ID:        "c1",
		Name:      "used",
		Arguments: message.NewArguments([]byte(`{}`)),
	})
	if result.IsError {
		t.Fatalf("Execute: %s", result.Content.Text())
	}

	if !view.Visible("used") {
		t.Fatal("the projection did not learn the tool was used: it is still hidden")
	}
	defs := view.Definitions()
	if len(defs) != 1 || defs[0].Name != "used" {
		t.Fatalf("definitions = %v, want [used]", defs)
	}
}

// TestARefusedCallDoesNotReachTheProjection is the reason recording lives in dispatch and
// not in middleware: a middleware wraps the whole chain, so it would record the attempt,
// and naming a forbidden tool would make it visible next round.
func TestARefusedCallDoesNotReachTheProjection(t *testing.T) {
	secret := tool.TextTool(message.ToolDefinition{
		Name:        "secret",
		InputSchema: []byte(`{"type":"object"}`),
	}, func(context.Context, string) (string, error) { return "ok", nil })

	catalog, err := tool.NewRegistry([]tool.Source{tooltest.Source(secret)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	view := mustProjection(t, catalog).NewView()
	executor := dispatch.New(catalog)

	grant := policy.ToolSet{Allow: []string{"other"}}
	binding := tool.Bind(tooltest.Session("s1"), policy.Grant{Tools: grant}).WithView(view)
	result := executor.Execute(context.Background(), binding, message.ToolCall{
		ID:        "c1",
		Name:      "secret",
		Arguments: message.NewArguments([]byte(`{}`)),
	})
	if !result.IsError {
		t.Fatal("a disallowed tool was executed")
	}
	if view.Visible("secret") {
		t.Fatal("a refused call made the tool visible")
	}
}

// TestRecordingWithoutAViewIsANoOp keeps one chain serving sessions with and without
// discovery: a static host binds no projection view, and recording must not panic.
func TestRecordingWithoutAViewIsANoOp(t *testing.T) {
	echo := tool.TextTool(message.ToolDefinition{
		Name:        "echo",
		InputSchema: []byte(`{"type":"object"}`),
	}, func(_ context.Context, args string) (string, error) { return args, nil })

	catalog, err := tool.NewRegistry([]tool.Source{tooltest.Source(echo)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	executor := dispatch.New(catalog)
	binding := tool.Bind(tooltest.Session("s1"), policy.Grant{})

	result := executor.Execute(context.Background(), binding, message.ToolCall{
		ID:        "c1",
		Name:      "echo",
		Arguments: message.NewArguments([]byte(`{}`)),
	})
	if result.IsError {
		t.Fatalf("Execute: %s", result.Content.Text())
	}
}

// TestToolSearchThroughTheWholeChain runs the discovery tool the way a deployment would:
// dispatch → middleware → tool_search → projection → next round's definitions.
func TestToolSearchThroughTheWholeChain(t *testing.T) {
	reader := tool.TextTool(message.ToolDefinition{
		Name:        "read_file",
		Description: "read a file from the session's roots",
		InputSchema: []byte(`{"type":"object"}`),
	}, func(context.Context, string) (string, error) { return "content", nil })

	search := projection.SearchTool{}
	catalog, err := tool.NewRegistry([]tool.Source{tooltest.Source(reader, search)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	view := mustProjection(t, catalog).NewView()
	executor := dispatch.New(catalog)
	binding := tool.Bind(tooltest.Session("s1"), policy.Grant{}).WithView(view)

	// tool_search is always exposed, so the model can call it on turn one.
	found := false
	for _, def := range view.Definitions() {
		if def.Name == projection.ToolName {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool_search is not visible: %v", view.Definitions())
	}

	result := executor.Execute(context.Background(), binding, message.ToolCall{
		ID:        "c1",
		Name:      projection.ToolName,
		Arguments: message.NewArguments([]byte(`{"query":"read a file"}`)),
	})
	if result.IsError {
		t.Fatalf("tool_search: %s", result.Content.Text())
	}
	if !strings.Contains(result.Content.Text()+dataText(result.Content), "read_file") {
		t.Fatalf("answer does not name the hit: %s", dataText(result.Content))
	}
	if !view.Visible("read_file") {
		t.Fatal("the discovered tool is not visible next round")
	}
}

func mustProjection(t *testing.T, catalog tool.Catalog) *projection.Projection {
	t.Helper()
	p, err := projection.New(catalog, projection.Policy{
		Default: projection.ExposureDeferred,
		Exposures: map[string]projection.Exposure{
			projection.ToolName: projection.ExposureAlways,
		},
	})
	if err != nil {
		t.Fatalf("projection.New: %v", err)
	}
	return p
}

// dataText is the payload of a structured result: tool_search answers with a data part,
// so Content.Text is empty by design.
func dataText(content message.Content) string {
	for _, part := range content.Parts {
		if data, ok := part.(message.DataPart); ok {
			return string(data.Value)
		}
	}
	return ""
}

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	sdktool "github.com/GizClaw/flowcraft/core/tool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// emptySchema is the fallback input schema for a server that omits one.
// The tool contract requires a JSON object, and "accepts anything" is
// the honest reading of a missing schema.
var emptySchema = json.RawMessage(`{"type":"object"}`)

// adaptedTool presents one MCP server tool as a core/tool.Tool. It holds
// the qualified (namespaced) name it was registered under plus the
// definition captured at discovery time, so Definition() is a pure
// accessor — no network, no error, matching the Catalog contract that
// every LLM turn depends on.
type adaptedTool struct {
	server *server
	def    message.ToolDefinition
	// remote is the tool's name on the server, which differs from
	// def.Name whenever a prefix is applied. tools/call must carry the
	// remote name.
	remote string
	meta   sdktool.ToolMeta
}

var (
	_ sdktool.Tool         = (*adaptedTool)(nil)
	_ sdktool.ToolMetadata = (*adaptedTool)(nil)
)

// newAdaptedTool projects an MCP tool descriptor onto the local
// contract. qualified is the name the registry will key on.
func newAdaptedTool(srv *server, qualified string, mt *mcpsdk.Tool) *adaptedTool {
	return &adaptedTool{
		server: srv,
		remote: mt.Name,
		def: message.ToolDefinition{
			Name:        qualified,
			Description: describe(mt),
			InputSchema: normalizeSchema(mt.InputSchema),
		},
		meta: metaFromAnnotations(mt.Annotations),
	}
}

func (a *adaptedTool) Definition() message.ToolDefinition { return a.def }

func (a *adaptedTool) Metadata() sdktool.ToolMeta { return a.meta }

// Execute forwards the call to the server as tools/call and renders the
// result into the single string the tool contract returns.
//
// Two failure modes are distinguished deliberately. A transport or
// protocol failure means the server is unreachable or broke the
// contract, so it surfaces as errdefs.NotAvailable tagged with the
// server name — that is what makes one dead server degrade only its own
// tools. A result carrying isError is the *tool* failing, which the
// model is expected to see and self-correct from, so the rendered
// content becomes the error message verbatim.
func (a *adaptedTool) Execute(ctx context.Context, arguments string) (string, error) {
	args, err := decodeArguments(arguments)
	if err != nil {
		return "", err
	}
	session, err := a.server.currentSession()
	if err != nil {
		return "", err
	}
	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      a.remote,
		Arguments: args,
	})
	if err != nil {
		return "", errdefs.NotAvailablef(
			"mcp: server %q: call tool %q: %v", a.server.name, a.remote, err)
	}
	rendered := renderResult(res)
	if res.IsError {
		if rendered == "" {
			rendered = fmt.Sprintf("mcp tool %q reported an error with no detail", a.remote)
		}
		return "", fmt.Errorf("%s", rendered)
	}
	return rendered, nil
}

// decodeArguments turns the contract's JSON string into the `any` the
// go-sdk marshals back onto the wire. An empty string is the "no
// arguments" case the tool suite exercises and maps to an empty object,
// not an error, because plenty of MCP tools take no input.
func decodeArguments(arguments string) (any, error) {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, errdefs.Validationf("mcp: parse arguments: %v", err)
	}
	if decoded == nil {
		return map[string]any{}, nil
	}
	return decoded, nil
}

// describe prefers the annotation title as a lead-in when the server
// supplies one, because a bare description often omits the human name
// the model benefits from seeing.
func describe(mt *mcpsdk.Tool) string {
	if mt.Annotations != nil && mt.Annotations.Title != "" && mt.Description != "" {
		return mt.Annotations.Title + ": " + mt.Description
	}
	if mt.Description != "" {
		return mt.Description
	}
	if mt.Annotations != nil {
		return mt.Annotations.Title
	}
	return ""
}

// normalizeSchema coerces the go-sdk's `any`-typed input schema into the
// raw JSON object the Definition contract requires. The go-sdk documents
// that a client-side schema arrives as map[string]any, but servers are
// free to send anything, so every shape that is not a JSON object
// degrades to the permissive empty schema rather than producing a
// Definition that fails Validate().
func normalizeSchema(schema any) json.RawMessage {
	switch typed := schema.(type) {
	case nil:
		return emptySchema
	case json.RawMessage:
		return objectOrEmpty(typed)
	case []byte:
		return objectOrEmpty(typed)
	case string:
		return objectOrEmpty([]byte(typed))
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return emptySchema
		}
		return objectOrEmpty(encoded)
	}
}

func objectOrEmpty(raw []byte) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
		return emptySchema
	}
	return json.RawMessage(trimmed)
}

// metaFromAnnotations maps MCP tool hints onto the local ToolMeta.
//
// SelfTimeout is always true: an MCP call already runs under the
// caller's context deadline and the transport's own timeout, so the
// timeout middleware's default would only add a redundant second
// deadline reporting a less specific error. A host that wants a hard
// bound on a particular MCP tool still gets one via the middleware's
// per-tool table, which outranks this claim.
//
// Of the MCP annotations only ReadOnlyHint is actionable today: it
// answers "is re-invoking this safe", which is exactly what
// MutatesState gates. The spec defaults ReadOnlyHint to false and the
// local contract's conservative default is likewise "assume it
// mutates", so an absent annotation lands on MutatesState=true without
// special casing. RateLimit stays zero — MCP has no equivalent hint,
// and inventing one would be a policy decision the host owns.
func metaFromAnnotations(ann *mcpsdk.ToolAnnotations) sdktool.ToolMeta {
	return sdktool.ToolMeta{
		MutatesState: ann == nil || !ann.ReadOnlyHint,
		SelfTimeout:  true,
	}
}

// renderResult flattens an MCP result into one string.
//
// The rule is deterministic and documented in the package overview:
// content parts render in order, joined by newlines; text parts
// contribute their text verbatim; every other part contributes its JSON
// wire form so no information is silently dropped. When there is no
// content at all but there is structured content, the structured value
// is rendered instead — servers using output schemas commonly populate
// only that field.
func renderResult(res *mcpsdk.CallToolResult) string {
	if res == nil {
		return ""
	}
	parts := make([]string, 0, len(res.Content))
	for _, content := range res.Content {
		if rendered := renderContent(content); rendered != "" {
			parts = append(parts, rendered)
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if encoded, err := json.Marshal(res.StructuredContent); err == nil {
			return string(encoded)
		}
	}
	return strings.Join(parts, "\n")
}

func renderContent(content mcpsdk.Content) string {
	if content == nil {
		return ""
	}
	if text, ok := content.(*mcpsdk.TextContent); ok {
		return text.Text
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return fmt.Sprintf("<unrenderable %T content>", content)
	}
	return string(encoded)
}

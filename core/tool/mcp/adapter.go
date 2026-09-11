package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
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

// Execute forwards the call to the server as tools/call and maps the
// result onto canonical content parts: text, image, audio, resource
// links, and embedded resources keep their native representation
// instead of being flattened into text at the MCP boundary.
//
// Two failure modes are distinguished deliberately. A transport or
// protocol failure means the server is unreachable or broke the
// contract, so it surfaces as errdefs.NotAvailable tagged with the
// server name — that is what makes one dead server degrade only its own
// tools. A result carrying isError is the *tool* failing, which the
// model is expected to see and self-correct from, so the content's
// rendered form becomes the error message verbatim.
func (a *adaptedTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	args, err := decodeArguments(arguments)
	if err != nil {
		return message.Content{}, err
	}
	session, err := a.server.currentSession()
	if err != nil {
		return message.Content{}, err
	}
	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      a.remote,
		Arguments: args,
	})
	if err != nil {
		return message.Content{}, errdefs.NotAvailablef(
			"mcp: server %q: call tool %q: %v", a.server.name, a.remote, err)
	}
	content := resultContent(res)
	if res != nil && res.IsError {
		rendered := content.Text()
		if rendered == "" {
			rendered = fmt.Sprintf("mcp tool %q reported an error with no detail", a.remote)
		}
		return message.Content{}, fmt.Errorf("%s", rendered)
	}
	return content, nil
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

// resultContent maps an MCP tool result onto canonical message parts.
//
// Text, image, and audio content map to their typed parts; a resource
// link becomes a [message.FilePart] reference; an embedded resource
// contributes its text or its typed media payload. Anything the local
// model has no typed home for — including forward-compatible content
// types this client does not know yet — keeps its JSON wire form as a
// data part (or a text part when it is not a JSON object), so no
// information is silently dropped.
//
// When the server returns no content at all but does return structured
// content, the structured value is carried instead: servers using
// output schemas commonly populate only that field.
func resultContent(res *mcpsdk.CallToolResult) message.Content {
	if res == nil {
		return message.NewTextContent("")
	}
	parts := make([]message.Part, 0, len(res.Content))
	for _, content := range res.Content {
		if part, ok := contentPart(content); ok {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if encoded, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, jsonPart(encoded))
		}
	}
	if len(parts) == 0 {
		return message.NewTextContent("")
	}
	return message.Content{Parts: parts}
}

// contentPart maps one MCP content block onto a canonical part. The
// boolean is false only when the block is nil or cannot be encoded.
func contentPart(content mcpsdk.Content) (message.Part, bool) {
	if content == nil {
		return nil, false
	}
	switch typed := content.(type) {
	case *mcpsdk.TextContent:
		// An empty block carries nothing; treating it as absent keeps the
		// structured-content fallback below reachable.
		if typed.Text == "" {
			return nil, false
		}
		return message.TextPart{Text: typed.Text}, true
	case *mcpsdk.ImageContent:
		if source, err := media.NewImageBytes(typed.Data, typed.MIMEType); err == nil {
			return message.ImagePart{Source: source}, true
		}
	case *mcpsdk.AudioContent:
		if source, err := media.NewAudioBytes(typed.Data, typed.MIMEType); err == nil {
			return message.AudioPart{Source: source}, true
		}
	case *mcpsdk.ResourceLink:
		if typed.URI != "" {
			return message.FilePart{
				URI:       typed.URI,
				MediaType: typed.MIMEType,
				Name:      typed.Name,
			}, true
		}
	case *mcpsdk.EmbeddedResource:
		if part, ok := resourceContentsPart(typed.Resource); ok {
			return part, true
		}
	}
	// No typed home: keep the wire form rather than dropping the block.
	encoded, err := json.Marshal(content)
	if err != nil {
		return nil, false
	}
	return jsonPart(encoded), true
}

// jsonPart carries encoded JSON as a structured data part when it is a
// JSON object, and as text otherwise ([message.DataPart] requires an
// object). Nothing is dropped either way.
func jsonPart(raw []byte) message.Part {
	content, err := message.NewJSONContent(raw)
	if err != nil {
		return message.TextPart{Text: strings.TrimSpace(string(raw))}
	}
	return content.Parts[0]
}

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	sdktool "github.com/GizClaw/flowcraft/core/tool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Resource bridge tool names, namespaced like any other tool by the
// server prefix (filesystem__list_resources, filesystem__read_resource).
const (
	listResourcesToolName = "list_resources"
	readResourceToolName  = "read_resource"
)

type resourceKind uint8

const (
	resourceList resourceKind = iota
	resourceRead
)

// resourceTool adapts one MCP resource operation to the local tool
// contract, so resources flow through the same registry, exposure,
// approval, and middleware machinery as every other tool.
type resourceTool struct {
	server *server
	def    message.ToolDefinition
	kind   resourceKind
}

var _ sdktool.Tool = (*resourceTool)(nil)

func (r *resourceTool) Definition() message.ToolDefinition { return r.def }

func (r *resourceTool) Execute(ctx context.Context, arguments string) (string, error) {
	session, err := r.server.currentSession()
	if err != nil {
		return "", err
	}
	switch r.kind {
	case resourceList:
		return r.list(ctx, session)
	case resourceRead:
		return r.read(ctx, session, arguments)
	default:
		return "", errdefs.Internalf("mcp: unknown resource tool kind")
	}
}

func (r *resourceTool) list(ctx context.Context, session *mcpsdk.ClientSession) (string, error) {
	res, err := session.ListResources(ctx, nil)
	if err != nil {
		return "", errdefs.NotAvailablef(
			"mcp: server %q: list resources: %v", r.server.name, err)
	}
	raw, err := json.Marshal(renderResourceList(res))
	if err != nil {
		return "", errdefs.Internalf(
			"mcp: server %q: encode resource list: %v", r.server.name, err)
	}
	return string(raw), nil
}

func (r *resourceTool) read(ctx context.Context, session *mcpsdk.ClientSession, arguments string) (string, error) {
	var args struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", errdefs.Validationf(
			"mcp: %s: parse arguments: %v", readResourceToolName, err)
	}
	if strings.TrimSpace(args.URI) == "" {
		return "", errdefs.Validationf(
			"mcp: %s: uri is required", readResourceToolName)
	}
	res, err := session.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: args.URI})
	if err != nil {
		return "", errdefs.NotAvailablef(
			"mcp: server %q: read resource %q: %v", r.server.name, args.URI, err)
	}
	raw, err := json.Marshal(renderResourceContents(res.Contents))
	if err != nil {
		return "", errdefs.Internalf(
			"mcp: server %q: encode resource contents: %v", r.server.name, err)
	}
	return string(raw), nil
}

type resourceMeta struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mime_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

func renderResourceList(res *mcpsdk.ListResourcesResult) []resourceMeta {
	if res == nil {
		return []resourceMeta{}
	}
	out := make([]resourceMeta, 0, len(res.Resources))
	for _, r := range res.Resources {
		if r == nil {
			continue
		}
		out = append(out, resourceMeta{
			URI:         r.URI,
			Name:        r.Name,
			Title:       r.Title,
			Description: r.Description,
			MIMEType:    r.MIMEType,
			Size:        r.Size,
		})
	}
	return out
}

type renderedResource struct {
	URI        string `json:"uri"`
	MIMEType   string `json:"mime_type,omitempty"`
	Text       string `json:"text,omitempty"`
	BlobBase64 string `json:"blob_base64,omitempty"`
}

func renderResourceContents(contents []*mcpsdk.ResourceContents) []renderedResource {
	if len(contents) == 0 {
		return []renderedResource{}
	}
	out := make([]renderedResource, 0, len(contents))
	for _, c := range contents {
		if c == nil {
			continue
		}
		item := renderedResource{URI: c.URI, MIMEType: c.MIMEType, Text: c.Text}
		if len(c.Blob) > 0 {
			item.BlobBase64 = base64.StdEncoding.EncodeToString(c.Blob)
		}
		out = append(out, item)
	}
	return out
}

func listResourcesDefinition(qualified string) message.ToolDefinition {
	return message.ToolDefinition{
		Name:        qualified,
		Description: "List the resources this MCP server exposes (uri, name, description, mime type, size).",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func readResourceDefinition(qualified string) message.ToolDefinition {
	return message.DefineSchema(
		qualified,
		"Read one resource from this MCP server by URI. Returns the resource contents as JSON; binary blobs are base64-encoded.",
		message.ToolProperty("uri", "string", "the resource URI to read"),
	).Required("uri").Build()
}

// resourceToolSpec pairs a resource bridge remote name with its tool.
type resourceToolSpec struct {
	remote string
	tool   sdktool.Tool
}

func resourceToolSpecs(srv *server) []resourceToolSpec {
	return []resourceToolSpec{
		{
			remote: listResourcesToolName,
			tool: &resourceTool{
				server: srv,
				kind:   resourceList,
				def:    listResourcesDefinition(srv.qualify(listResourcesToolName)),
			},
		},
		{
			remote: readResourceToolName,
			tool: &resourceTool{
				server: srv,
				kind:   resourceRead,
				def:    readResourceDefinition(srv.qualify(readResourceToolName)),
			},
		},
	}
}

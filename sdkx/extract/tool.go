package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// FetchURLTool implements tool.Tool for URL content extraction.
type FetchURLTool struct {
	extractor Extractor
}

// NewFetchURLTool creates a new FetchURLTool.
func NewFetchURLTool(extractor Extractor) *FetchURLTool {
	return &FetchURLTool{extractor: extractor}
}

type fetchURLArgs struct {
	URL      string `json:"url"`
	MaxChars int    `json:"max_characters,omitempty"`
	Format   string `json:"format,omitempty"`
}

// Definition returns the tool definition for LLM function-calling.
func (t *FetchURLTool) Definition() model.ToolDefinition {
	return tool.DefineSchema("fetch_url",
		"Extract clean text content from a URL. Supports web pages, YouTube videos, podcasts, and more.",
		tool.Property("url", "string", "The URL to extract content from"),
		tool.Property("max_characters", "integer", "Maximum number of characters to extract (optional)"),
		tool.EnumProperty("format", "string", "Output format: 'text' (default) or 'markdown'", "text", "markdown"),
	).Required("url").Build()
}

// Execute runs the tool with given arguments, passing per-call options
// to the underlying extractor without creating a new instance.
func (t *FetchURLTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args fetchURLArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.URL == "" {
		return "", fmt.Errorf("url is required")
	}

	var opts []Option
	if args.MaxChars > 0 {
		opts = append(opts, WithMaxCharacters(args.MaxChars))
	}
	if args.Format != "" {
		switch strings.ToLower(args.Format) {
		case "markdown":
			opts = append(opts, WithFormat(FormatMarkdown))
		default:
			opts = append(opts, WithFormat(FormatText))
		}
	}

	content, err := t.extractor.Extract(ctx, args.URL, opts...)
	if err != nil {
		return "", fmt.Errorf("extraction failed: %w", err)
	}

	result := map[string]any{
		"url":       content.FinalURL,
		"title":     content.Title,
		"content":   content.Content,
		"truncated": content.Truncated,
	}

	if content.Diagnostics != nil {
		result["method"] = content.Diagnostics.Strategy
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}

	return string(resultJSON), nil
}

// Register registers the fetch_url tool to the tool registry.
func Register(reg *tool.Registry, extractor Extractor) {
	reg.Register(NewFetchURLTool(extractor))
}

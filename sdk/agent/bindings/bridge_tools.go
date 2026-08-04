package bindings

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/rs/xid"
)

type toolBridgeConfig struct {
	allowed  map[string]bool
	allowAll bool
}

// ToolBridgeOption configures NewToolBridge.
type ToolBridgeOption func(*toolBridgeConfig)

// WithToolAllowAll allows calling any tool in the catalog.
// Use only when scripts are fully trusted.
func WithToolAllowAll() ToolBridgeOption {
	return func(c *toolBridgeConfig) { c.allowAll = true }
}

// WithAllowedToolNames restricts script-visible tools; names must match catalog entries.
func WithAllowedToolNames(names ...string) ToolBridgeOption {
	return func(c *toolBridgeConfig) {
		if c.allowed == nil {
			c.allowed = make(map[string]bool)
		}
		for _, n := range names {
			c.allowed[n] = true
		}
	}
}

// NewToolBridge exposes tool execution to scripts as global "tools":
//   - call(name, argumentsJSON) -> { content, is_error, tool_call_id }
//   - callAll([{ name, arguments, id? }, ...]) -> same shape, in input
//     order, plus a "name" echo per entry. Concurrency comes from the
//     dispatcher's ExecuteAll. The optional id lets a script forward a
//     model-issued tool_call id verbatim — required when results feed
//     back into an LLM turn, where the provider matches tool_results
//     by call id; a fresh id is minted when absent
//   - list() -> []string (names the script is allowed to call)
//   - definitions() -> message.Definition wire JSON for the same allowed
//     set, ready to splice into a generate request's
//     input.content.intent.text.tools
//
// dispatcher executes the calls (typically a *tool.Executor assembled
// with the middleware the host wants — approval, timeouts, audit);
// catalog answers name lookups for allow-listing and list(). Splitting
// the two keeps the bridge aligned with the tool package's
// catalog/execution separation.
//
// Security: by default no tool is callable until WithAllowedToolNames or
// WithToolAllowAll is set. The allow-list applies per call: a denied
// entry in callAll gets an is_error result in place while the rest of
// the batch still runs.
func NewToolBridge(dispatcher tool.Dispatcher, catalog tool.Catalog, opts ...ToolBridgeOption) BindingFunc {
	cfg := &toolBridgeConfig{}
	for _, o := range opts {
		o(cfg)
	}
	deny := func(msg string) map[string]any {
		return map[string]any{"content": msg, "is_error": true, "tool_call_id": ""}
	}
	allowed := func(name string) (map[string]any, bool) {
		if dispatcher == nil || catalog == nil {
			return deny("tools: no dispatcher/catalog configured"), false
		}
		if !cfg.allowAll {
			if cfg.allowed == nil || !cfg.allowed[name] {
				return deny(fmt.Sprintf("tools: tool %q is not allowed for this script", name)), false
			}
		} else if _, ok := catalog.Get(name); !ok {
			return deny(fmt.Sprintf("tools: unknown tool %q", name)), false
		}
		return nil, true
	}
	resultMap := func(name string, res message.Result) map[string]any {
		return map[string]any{
			"content":      res.Content,
			"is_error":     res.IsError,
			"tool_call_id": res.CallID,
			"name":         name,
		}
	}
	return func(ctx context.Context) (string, any) {
		return "tools", map[string]any{
			"call": func(name string, argumentsJSON string) (map[string]any, error) {
				if denied, ok := allowed(name); !ok {
					return denied, nil
				}
				call := message.Call{
					ID:        xid.New().String(),
					Name:      name,
					Arguments: json.RawMessage(argumentsJSON),
				}
				return resultMap(name, dispatcher.Execute(ctx, call)), nil
			},
			"callAll": func(raw any) ([]map[string]any, error) {
				items, err := asAnyList(raw, "tools.callAll")
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, len(items))
				calls := make([]message.Call, 0, len(items))
				slots := make([]int, 0, len(items))
				for i, item := range items {
					spec, err := parseCallSpec(item, i)
					if err != nil {
						return nil, err
					}
					if denied, ok := allowed(spec.name); !ok {
						denied["name"] = spec.name
						out[i] = denied
						continue
					}
					id := spec.id
					if id == "" {
						id = xid.New().String()
					}
					calls = append(calls, message.Call{
						ID:        id,
						Name:      spec.name,
						Arguments: json.RawMessage(spec.arguments),
					})
					slots = append(slots, i)
				}
				results := dispatcherOrNil(dispatcher).ExecuteAll(ctx, calls)
				for j, res := range results {
					out[slots[j]] = resultMap(calls[j].Name, res)
				}
				return out, nil
			},
			"list": func() []string {
				if catalog == nil {
					return nil
				}
				defs := catalog.Definitions()
				out := make([]string, 0, len(defs))
				for _, d := range defs {
					if cfg.allowAll || cfg.allowed[d.Name] {
						out = append(out, d.Name)
					}
				}
				return out
			},
			"definitions": func() ([]any, error) {
				if catalog == nil {
					return nil, nil
				}
				defs := catalog.Definitions()
				out := make([]any, 0, len(defs))
				for _, d := range defs {
					if !cfg.allowAll && !cfg.allowed[d.Name] {
						continue
					}
					projected, err := toScriptJSON(d, "tools.definitions")
					if err != nil {
						return nil, err
					}
					out = append(out, projected)
				}
				return out, nil
			},
		}
	}
}

// callSpec is one parsed callAll entry. id may be empty (minted at
// batch assembly); arguments defaults to "{}" when the script omits
// it, matching the no-args convention of call("", "{}").
type callSpec struct {
	id        string
	name      string
	arguments string
}

func parseCallSpec(raw any, idx int) (callSpec, error) {
	var spec callSpec
	m, ok := raw.(map[string]any)
	if !ok {
		return spec, errdefs.Validationf("tools.callAll[%d]: expected an object, got %T", idx, raw)
	}
	name, ok := m["name"].(string)
	if !ok || name == "" {
		return spec, errdefs.Validationf("tools.callAll[%d]: name is required", idx)
	}
	spec.name = name
	if id, ok := m["id"]; ok && id != nil {
		s, ok := id.(string)
		if !ok {
			return spec, errdefs.Validationf("tools.callAll[%d].id: expected string, got %T", idx, id)
		}
		spec.id = s
	}
	spec.arguments = "{}"
	if args, ok := m["arguments"]; ok && args != nil {
		s, ok := args.(string)
		if !ok {
			return spec, errdefs.Validationf("tools.callAll[%d].arguments: expected JSON string, got %T", idx, args)
		}
		spec.arguments = s
	}
	return spec, nil
}

// nilDispatcher yields is_error results for every call, so a batch
// issued against a bridge built without a dispatcher degrades
// per-entry instead of panicking.
type nilDispatcher struct{}

func (nilDispatcher) Execute(_ context.Context, call message.Call) message.Result {
	return message.Result{CallID: call.ID, Content: "tools: no dispatcher/catalog configured", IsError: true}
}

func (d nilDispatcher) ExecuteAll(_ context.Context, calls []message.Call) []message.Result {
	out := make([]message.Result, len(calls))
	for i, c := range calls {
		out[i] = d.Execute(context.Background(), c)
	}
	return out
}

func dispatcherOrNil(d tool.Dispatcher) tool.Dispatcher {
	if d == nil {
		return nilDispatcher{}
	}
	return d
}

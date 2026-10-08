// Package hostmcp exposes the host primitives to plugins as a local MCP
// server. It owns the tool registry, the per-plugin bearer tokens and
// the built-in primitive set; the HTTP transport arrives in a later
// milestone through the official go-sdk StreamableHTTPHandler.
package hostmcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// ProtocolVersion is the host primitive protocol version reported by
// host_about.
const ProtocolVersion = 1

// Identity is the authenticated plugin behind one token.
type Identity struct {
	PluginID string
	Grants   GrantSet
}

// GrantSet is the set of manifest permissions of one plugin.
type GrantSet map[string]bool

// Has reports whether the grant is present.
func (g GrantSet) Has(grant string) bool { return g[grant] }

// SecretService backs secret_get / secret_set / secret_delete.
type SecretService interface {
	Get(ctx context.Context, pluginID, name string) (string, error)
	Set(ctx context.Context, pluginID, name, value string) error
	Delete(ctx context.Context, pluginID, name string) error
}

// ContextService backs workspace_current.
type ContextService interface {
	Workspace(ctx context.Context) (map[string]any, error)
}

// BrowserService backs open_url.
type BrowserService interface {
	OpenURL(ctx context.Context, url string) error
}

// InferenceService backs inference_upsert / inference_remove.
type InferenceService interface {
	Upsert(ctx context.Context, pluginID string, profile map[string]any) (string, error)
	Remove(ctx context.Context, pluginID, id string) error
}

// SessionService backs session_import / session_imported_sources.
type SessionService interface {
	Import(ctx context.Context, pluginID string, payload map[string]any) (map[string]any, error)
	ImportedSources(ctx context.Context, pluginID string) ([]string, error)
}

// TelemetryService backs telemetry_configure / telemetry_disable.
type TelemetryService interface {
	Configure(ctx context.Context, pluginID, endpoint string, headers map[string]string) error
	Disable(ctx context.Context, pluginID string) error
}

// EventService backs emit_event.
type EventService interface {
	Emit(ctx context.Context, pluginID, subject string, payload any) error
}

// Services is the app-provided behavior of the standard primitives. A
// nil field removes its primitives from the exposed set (fail closed).
type Services struct {
	Secrets   SecretService
	Context   ContextService
	Browser   BrowserService
	Inference InferenceService
	Sessions  SessionService
	Telemetry TelemetryService
	Events    EventService
}

// Call is one primitive invocation.
type Call struct {
	Identity Identity
	Params   map[string]any
}

// String reads a required string parameter.
func (c Call) String(name string) (string, error) {
	value, ok := c.Params[name].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", errdefs.Validationf("hostmcp: parameter %q is required", name)
	}
	return value, nil
}

// Tool is one registered primitive.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// Grant is required to call the tool; empty means always allowed.
	Grant string
	// Standard marks a built-in primitive.
	Standard bool
	Handler  func(ctx context.Context, call Call) (any, error)
}

// Registry holds the exposed primitives.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds one tool; name, handler and schema are required.
func (r *Registry) Register(tool Tool) error {
	if strings.TrimSpace(tool.Name) == "" {
		return errdefs.Validationf("hostmcp: tool name is required")
	}
	if tool.Handler == nil {
		return errdefs.Validationf("hostmcp: tool %q has no handler", tool.Name)
	}
	if len(tool.InputSchema) == 0 {
		tool.InputSchema = json.RawMessage(`{"type":"object"}`)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, duplicate := r.tools[tool.Name]; duplicate {
		return errdefs.Conflictf("hostmcp: tool %q already registered", tool.Name)
	}
	r.tools[tool.Name] = tool
	return nil
}

// Lookup returns one tool.
func (r *Registry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

// Tools returns every tool in name order.
func (r *Registry) Tools() []Tool {
	r.mu.RLock()
	out := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		out = append(out, tool)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Tokens mints and revokes per-plugin bearer tokens.
type Tokens struct {
	mu     sync.RWMutex
	byHash map[string]Identity
}

// NewTokens returns an empty token store.
func NewTokens() *Tokens {
	return &Tokens{byHash: make(map[string]Identity)}
}

// Mint issues one token for identity.
func (t *Tokens) Mint(identity Identity) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errdefs.Internal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	t.mu.Lock()
	t.byHash[token] = identity
	t.mu.Unlock()
	return token, nil
}

// Lookup resolves one token.
func (t *Tokens) Lookup(token string) (Identity, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	identity, ok := t.byHash[token]
	return identity, ok
}

// Revoke removes one token.
func (t *Tokens) Revoke(token string) {
	t.mu.Lock()
	delete(t.byHash, token)
	t.mu.Unlock()
}

// About is the host_about response.
type About struct {
	Protocol    int           `json:"protocol"`
	HostVersion string        `json:"host_version,omitempty"`
	Primitives  []string      `json:"primitives"`
	Denied      []DeniedEntry `json:"denied,omitempty"`
}

// DeniedEntry explains one primitive the caller cannot use.
type DeniedEntry struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"` // no_service | no_grant
}

// Standard registers the built-in primitive set backed by services.
// host_about is always registered and reports the primitives missing a
// service, plus the caller's missing grants.
func Standard(
	registry *Registry,
	services Services,
	hostVersion string,
) error {
	return StandardFiltered(registry, services, hostVersion, nil)
}

// StandardFiltered is Standard with a disabled-primitive list: a
// disabled tool is never exposed and is reported by host_about.
func StandardFiltered(
	registry *Registry,
	services Services,
	hostVersion string,
	disabled []string,
) error {
	disabledSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disabledSet[strings.TrimSpace(name)] = true
	}
	available := make([]string, 0, 13)
	denied := make([]DeniedEntry, 0)
	register := func(tool Tool) error {
		if disabledSet[tool.Name] {
			denied = append(denied, DeniedEntry{
				Tool: tool.Name, Reason: "disabled",
			})
			return nil
		}
		if err := registry.Register(tool); err != nil {
			return err
		}
		available = append(available, tool.Name)
		return nil
	}
	missing := func(name string) {
		denied = append(denied, DeniedEntry{Tool: name, Reason: "no_service"})
	}

	if services.Secrets != nil {
		secret := services.Secrets
		if err := register(Tool{Name: "secret_get", Standard: true, Grant: "secrets:auth",
			Description: "Read a secret from the calling plugin's namespace",
			Handler: func(ctx context.Context, call Call) (any, error) {
				name, err := call.String("name")
				if err != nil {
					return nil, err
				}
				value, err := secret.Get(ctx, call.Identity.PluginID, name)
				if err != nil {
					return nil, err
				}
				return map[string]any{"value": value}, nil
			}}); err != nil {
			return err
		}
		if err := register(Tool{Name: "secret_set", Standard: true, Grant: "secrets:auth",
			Description: "Store a secret in the calling plugin's namespace",
			Handler: func(ctx context.Context, call Call) (any, error) {
				name, err := call.String("name")
				if err != nil {
					return nil, err
				}
				value, err := call.String("value")
				if err != nil {
					return nil, err
				}
				return map[string]any{}, secret.Set(ctx, call.Identity.PluginID, name, value)
			}}); err != nil {
			return err
		}
		if err := register(Tool{Name: "secret_delete", Standard: true, Grant: "secrets:auth",
			Description: "Delete a secret from the calling plugin's namespace",
			Handler: func(ctx context.Context, call Call) (any, error) {
				name, err := call.String("name")
				if err != nil {
					return nil, err
				}
				return map[string]any{}, secret.Delete(ctx, call.Identity.PluginID, name)
			}}); err != nil {
			return err
		}
	} else {
		missing("secret_get")
		missing("secret_set")
		missing("secret_delete")
	}

	if services.Context != nil {
		if err := register(Tool{Name: "workspace_current", Standard: true,
			Description: "Read the host's active workspace",
			Handler: func(ctx context.Context, _ Call) (any, error) {
				return services.Context.Workspace(ctx)
			}}); err != nil {
			return err
		}
	} else {
		missing("workspace_current")
	}

	if services.Browser != nil {
		if err := register(Tool{Name: "open_url", Standard: true, Grant: "host:open_url",
			Description: "Open a URL in the system browser",
			Handler: func(ctx context.Context, call Call) (any, error) {
				url, err := call.String("url")
				if err != nil {
					return nil, err
				}
				return map[string]any{}, services.Browser.OpenURL(ctx, url)
			}}); err != nil {
			return err
		}
	} else {
		missing("open_url")
	}

	if services.Inference != nil {
		if err := register(Tool{Name: "inference_upsert", Standard: true, Grant: "inference:write",
			Description: "Create or update an inference profile",
			Handler: func(ctx context.Context, call Call) (any, error) {
				id, err := services.Inference.Upsert(ctx, call.Identity.PluginID, call.Params)
				if err != nil {
					return nil, err
				}
				return map[string]any{"id": id}, nil
			}}); err != nil {
			return err
		}
		if err := register(Tool{Name: "inference_remove", Standard: true, Grant: "inference:write",
			Description: "Remove one inference profile",
			Handler: func(ctx context.Context, call Call) (any, error) {
				id, err := call.String("id")
				if err != nil {
					return nil, err
				}
				return map[string]any{}, services.Inference.Remove(
					ctx, call.Identity.PluginID, id)
			}}); err != nil {
			return err
		}
	} else {
		missing("inference_upsert")
		missing("inference_remove")
	}

	if services.Sessions != nil {
		if err := register(Tool{Name: "session_import", Standard: true, Grant: "sessions:import",
			Description: "Import external sessions",
			Handler: func(ctx context.Context, call Call) (any, error) {
				return services.Sessions.Import(ctx, call.Identity.PluginID, call.Params)
			}}); err != nil {
			return err
		}
		if err := register(Tool{Name: "session_imported_sources", Standard: true, Grant: "sessions:import",
			Description: "List imported session sources",
			Handler: func(ctx context.Context, call Call) (any, error) {
				sources, err := services.Sessions.ImportedSources(
					ctx, call.Identity.PluginID)
				if err != nil {
					return nil, err
				}
				return map[string]any{"sources": sources}, nil
			}}); err != nil {
			return err
		}
	} else {
		missing("session_import")
		missing("session_imported_sources")
	}

	if services.Telemetry != nil {
		if err := register(Tool{Name: "telemetry_configure", Standard: true, Grant: "telemetry:export",
			Description: "Point the plugin's OTLP export at a collector",
			Handler: func(ctx context.Context, call Call) (any, error) {
				endpoint, err := call.String("endpoint")
				if err != nil {
					return nil, err
				}
				headers := map[string]string{}
				if raw, ok := call.Params["headers"].(map[string]any); ok {
					for key, value := range raw {
						if text, ok := value.(string); ok {
							headers[key] = text
						}
					}
				}
				return map[string]any{}, services.Telemetry.Configure(
					ctx, call.Identity.PluginID, endpoint, headers)
			}}); err != nil {
			return err
		}
		if err := register(Tool{Name: "telemetry_disable", Standard: true, Grant: "telemetry:export",
			Description: "Disable the plugin's OTLP export",
			Handler: func(ctx context.Context, call Call) (any, error) {
				return map[string]any{}, services.Telemetry.Disable(
					ctx, call.Identity.PluginID)
			}}); err != nil {
			return err
		}
	} else {
		missing("telemetry_configure")
		missing("telemetry_disable")
	}

	if services.Events != nil {
		if err := register(Tool{Name: "emit_event", Standard: true, Grant: "events:emit",
			Description: "Publish one host event",
			Handler: func(ctx context.Context, call Call) (any, error) {
				subject, err := call.String("subject")
				if err != nil {
					return nil, err
				}
				return map[string]any{}, services.Events.Emit(
					ctx, call.Identity.PluginID, subject, call.Params["payload"])
			}}); err != nil {
			return err
		}
	} else {
		missing("emit_event")
	}

	available = append(available, "host_about")
	sort.Strings(available)
	return registry.Register(Tool{
		Name:        "host_about",
		Standard:    true,
		Description: "Report the host protocol, exposed primitives and denied primitives",
		Handler: func(_ context.Context, call Call) (any, error) {
			out := About{
				Protocol:    ProtocolVersion,
				HostVersion: hostVersion,
				Primitives:  available,
				Denied:      append([]DeniedEntry(nil), denied...),
			}
			for _, tool := range registry.Tools() {
				if tool.Grant == "" || call.Identity.Grants.Has(tool.Grant) {
					continue
				}
				out.Denied = append(out.Denied, DeniedEntry{
					Tool: tool.Name, Reason: "no_grant",
				})
			}
			sort.Slice(out.Denied, func(i, j int) bool {
				return out.Denied[i].Tool < out.Denied[j].Tool
			})
			return out, nil
		},
	})
}

// ServiceRegistry holds named service implementations so craft.yaml can
// select them with host_tools.services.<family>.impl.
type ServiceRegistry struct {
	mu     sync.RWMutex
	byName map[string]map[string]any
}

// NewServiceRegistry returns an empty registry.
func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{byName: make(map[string]map[string]any)}
}

// Register adds one named implementation for a family.
func (r *ServiceRegistry) Register(family, name string, impl any) error {
	family = strings.TrimSpace(family)
	name = strings.TrimSpace(name)
	if family == "" || name == "" || impl == nil {
		return errdefs.Validationf(
			"hostmcp: service registration requires family, name and impl")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byName[family] == nil {
		r.byName[family] = make(map[string]any)
	}
	if _, duplicate := r.byName[family][name]; duplicate {
		return errdefs.Conflictf(
			"hostmcp: service %s.%s already registered", family, name)
	}
	r.byName[family][name] = impl
	return nil
}

// Lookup returns one named implementation.
func (r *ServiceRegistry) Lookup(family, name string) (any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	impl, ok := r.byName[family][name]
	return impl, ok
}

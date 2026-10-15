package craft

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/craft/hostmcp"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// HostServices is the capability interface supplying the behavior of
// the built-in host primitives.
type HostServices interface {
	HostServiceSet() hostmcp.Services
}

// ServiceRegistrar lets a capability register named service
// implementations that craft.yaml selects by name.
type ServiceRegistrar interface {
	RegisterServices(*hostmcp.ServiceRegistry) error
}

// hostMCPState wires the host MCP server to the plugin host.
type hostMCPState struct {
	server *hostmcp.Server
	tokens *hostmcp.Tokens
	mu     sync.Mutex
	// byID records what was minted for one plugin's process as digests
	// ([hostmcp.Token]), never as the secrets themselves: revocation is
	// the only thing this map is for, and it must not be a second place
	// a replayable token lives. The record names the generation of the
	// process the tokens were minted for, because the grants a token
	// carries are the ones the manifest asked for when that process was
	// started: a process of a later generation replaces them (see
	// [hostMCPState.mint]).
	byID map[string]mintedTokens
}

// mintedTokens is one plugin's live host tokens, with the generation of
// the process they were minted for.
type mintedTokens struct {
	generation uint64
	tokens     []hostmcp.Token
}

// newHostMCP builds the server when the craft declares host services.
func newHostMCP(
	services hostmcp.Services,
	disabled []string,
	hostVersion string,
) (*hostMCPState, error) {
	registry := hostmcp.NewRegistry()
	if err := hostmcp.StandardFiltered(
		registry, services, hostVersion, disabled); err != nil {
		return nil, err
	}
	tokens := hostmcp.NewTokens()
	server, err := hostmcp.NewServer(hostmcp.ServerOptions{
		Registry:    registry,
		Tokens:      tokens,
		HostVersion: hostVersion,
		Audit:       hostAudit,
		RateLimit:   20,
	})
	if err != nil {
		return nil, err
	}
	return &hostMCPState{
		server: server,
		tokens: tokens,
		byID:   map[string]mintedTokens{},
	}, nil
}

// hostAudit records one primitive call outcome without payloads or
// secrets.
func hostAudit(event hostmcp.AuditEvent) {
	telemetry.Info(context.Background(), fmt.Sprintf(
		"hostmcp call plugin=%s tool=%s outcome=%s",
		event.PluginID, event.Tool, event.Outcome))
}

// applyServiceBindings resolves host_tools.services.<family>.impl
// against the named implementations registered by capabilities.
func applyServiceBindings(
	bindings map[string]ServiceBinding,
	named *hostmcp.ServiceRegistry,
	services *hostmcp.Services,
) error {
	for family, binding := range bindings {
		impl, ok := named.Lookup(family, binding.Impl)
		if !ok {
			return errdefs.Validationf(
				"craft: host_tools.services.%s impl %q is not registered",
				family, binding.Impl)
		}
		switch family {
		case "secrets":
			service, ok := impl.(hostmcp.SecretService)
			if !ok {
				return errdefs.Validationf(
					"craft: service %s.%s is %T, want SecretService",
					family, binding.Impl, impl)
			}
			services.Secrets = service
		case "workspace":
			service, ok := impl.(hostmcp.ContextService)
			if !ok {
				return errdefs.Validationf(
					"craft: service %s.%s is %T, want ContextService",
					family, binding.Impl, impl)
			}
			services.Context = service
		case "browser":
			service, ok := impl.(hostmcp.BrowserService)
			if !ok {
				return errdefs.Validationf(
					"craft: service %s.%s is %T, want BrowserService",
					family, binding.Impl, impl)
			}
			services.Browser = service
		case "inference":
			service, ok := impl.(hostmcp.InferenceService)
			if !ok {
				return errdefs.Validationf(
					"craft: service %s.%s is %T, want InferenceService",
					family, binding.Impl, impl)
			}
			services.Inference = service
		case "sessions":
			service, ok := impl.(hostmcp.SessionService)
			if !ok {
				return errdefs.Validationf(
					"craft: service %s.%s is %T, want SessionService",
					family, binding.Impl, impl)
			}
			services.Sessions = service
		case "telemetry":
			service, ok := impl.(hostmcp.TelemetryService)
			if !ok {
				return errdefs.Validationf(
					"craft: service %s.%s is %T, want TelemetryService",
					family, binding.Impl, impl)
			}
			services.Telemetry = service
		case "events":
			service, ok := impl.(hostmcp.EventService)
			if !ok {
				return errdefs.Validationf(
					"craft: service %s.%s is %T, want EventService",
					family, binding.Impl, impl)
			}
			services.Events = service
		default:
			return errdefs.Validationf(
				"craft: host_tools.services has unknown family %q", family)
		}
	}
	return nil
}

// mint issues one token for a plugin and records it for revocation. The
// secret comes back to hand to the process; the digest is what stays
// here. A mint failure keeps its cause: the caller reports it, and
// "mint plugin token" alone would hide whether the entropy source or
// the store was the problem.
//
// generation is the process this token is for. A process of a new
// generation replaces what the previous one held: its tokens are
// revoked and the cached SDK server — which carries the grants of the
// plugin the first time it connected — is dropped, so an update that
// takes a permission away cannot leave the process of the old manifest
// holding it. That is the whole reason the tokens are accounted per
// generation rather than per plugin: "the record says this plugin may
// do X" and "the process running it may do X" are then the same
// statement.
func (s *hostMCPState) mint(entry plugin.Entry, generation uint64) (string, error) {
	grants := hostmcp.GrantSet{}
	for _, permission := range entry.Manifest.Permissions {
		grants[permission] = true
	}
	secret, token, err := s.tokens.Mint(hostmcp.Identity{
		PluginID: entry.ID,
		Grants:   grants,
	})
	if err != nil {
		return "", errdefs.Internal(
			fmt.Errorf("craft: mint plugin token for %s: %w", entry.ID, err))
	}
	s.mu.Lock()
	previous := s.byID[entry.ID]
	replaced := previous.generation != generation
	if replaced {
		s.byID[entry.ID] = mintedTokens{generation: generation}
	}
	record := s.byID[entry.ID]
	record.tokens = append(record.tokens, token)
	s.byID[entry.ID] = record
	s.mu.Unlock()
	for _, stale := range previous.tokens {
		s.tokens.Revoke(stale)
	}
	if replaced {
		s.server.Drop(entry.ID)
	}
	return secret, nil
}

// revokePlugin drops every token of one plugin (disable/update/close)
// and invalidates the cached SDK server so new grants are re-read.
func (s *hostMCPState) revokePlugin(id string) {
	s.mu.Lock()
	tokens := s.byID[id].tokens
	delete(s.byID, id)
	s.mu.Unlock()
	for _, token := range tokens {
		s.tokens.Revoke(token)
	}
	s.server.Drop(id)
}

// installPluginHooks wires token minting into every plugin process and
// revocation into every plugin stop.
func (c *Craft) installPluginHooks() {
	if c.plugins == nil || c.hostMCP == nil {
		return
	}
	host, ok := c.plugins.(*plugin.Host)
	if !ok {
		return
	}
	host.SetPluginHooks(func(
		entry plugin.Entry,
		generation uint64,
		env map[string]string,
	) error {
		endpoint := c.hostMCP.server.URL()
		if endpoint == "" {
			return errdefs.NotAvailablef(
				"craft: host MCP endpoint is not started")
		}
		token, err := c.hostMCP.mint(entry, generation)
		if err != nil {
			return err
		}
		env["CRAFT_HOST_MCP_URL"] = endpoint
		env["CRAFT_PLUGIN_TOKEN"] = token
		return nil
	}, c.hostMCP.revokePlugin)
}

// hostServiceSet merges the HostServices capabilities in registration
// order; later capabilities override earlier fields.
func hostServiceSet(caps []Capability) hostmcp.Services {
	var services hostmcp.Services
	for _, capability := range caps {
		contributor, ok := capability.(HostServices)
		if !ok {
			continue
		}
		provided := contributor.HostServiceSet()
		if provided.Secrets != nil {
			services.Secrets = provided.Secrets
		}
		if provided.Context != nil {
			services.Context = provided.Context
		}
		if provided.Browser != nil {
			services.Browser = provided.Browser
		}
		if provided.Inference != nil {
			services.Inference = provided.Inference
		}
		if provided.Sessions != nil {
			services.Sessions = provided.Sessions
		}
		if provided.Telemetry != nil {
			services.Telemetry = provided.Telemetry
		}
		if provided.Events != nil {
			services.Events = provided.Events
		}
	}
	return services
}

// startHostMCP starts the endpoint before plugin processes spawn, so a
// plugin may call primitives during its handshake.
func (c *Craft) startHostMCP() error {
	if c.hostMCP == nil {
		return nil
	}
	return c.hostMCP.server.Start()
}

// closeHostMCP revokes every token and stops the endpoint.
func (c *Craft) closeHostMCP() error {
	if c.hostMCP == nil {
		return nil
	}
	c.hostMCP.mu.Lock()
	ids := make([]string, 0, len(c.hostMCP.byID))
	for id := range c.hostMCP.byID {
		ids = append(ids, id)
	}
	c.hostMCP.mu.Unlock()
	for _, id := range ids {
		c.hostMCP.revokePlugin(id)
	}
	return c.hostMCP.server.Close()
}

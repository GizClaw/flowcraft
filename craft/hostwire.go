package craft

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/craft/plugin"
	pluginserver "github.com/GizClaw/flowcraft/craft/plugin/server"
)

// HostPrimitives is the capability interface supplying the primitives a
// plugin may call back. Craft defines none of them: what a host has to
// offer is the application's decision, and this is where it is declared.
//
// Contributions merge in registration order. A duplicate primitive id
// fails craft.New rather than silently shadowing one capability's
// primitive with another's.
type HostPrimitives interface {
	HostPrimitives() []pluginserver.Primitive
}

// hostWire is the craft half of the plugin wire. There is no token
// bookkeeping here any more: the connection is the identity, so a process
// that is replaced or stopped loses its grants with its file descriptor.
type hostWire struct {
	server *pluginserver.Server
	mu     sync.Mutex
	closed bool
}

// hostAudit records one primitive call outcome without payloads or
// secrets.
func hostAudit(event pluginserver.AuditEvent) {
	telemetry.Info(context.Background(), fmt.Sprintf(
		"plugin host call plugin=%s primitive=%s outcome=%s",
		event.PluginID, event.Primitive, event.Outcome))
}

// newHostWire builds the wire server from the primitives the application
// contributes. A craft whose capabilities contribute none still gets one:
// host.about is the surface then, and a plugin can ask.
func newHostWire(
	primitives []pluginserver.Primitive,
	disabled []string,
	hostVersion string,
) (*hostWire, error) {
	registry, err := pluginserver.Catalog(primitives, hostVersion, disabled)
	if err != nil {
		return nil, err
	}
	server, err := pluginserver.NewServer(pluginserver.ServerOptions{
		Registry:    registry,
		HostVersion: hostVersion,
		Audit:       hostAudit,
		RateLimit:   20,
	})
	if err != nil {
		return nil, err
	}
	return &hostWire{server: server}, nil
}

// hostPrimitiveSet merges the HostPrimitives contributions in registration
// order. It is the application's whole catalog: craft adds host.about when
// the wire server is built, and nothing else.
func hostPrimitiveSet(caps []Capability) []pluginserver.Primitive {
	var primitives []pluginserver.Primitive
	for _, capability := range caps {
		contributor, ok := capability.(HostPrimitives)
		if !ok {
			continue
		}
		primitives = append(primitives, contributor.HostPrimitives()...)
	}
	return primitives
}

// installHostWire hands the wire server to the plugin host. It has to
// run before the host is started, because a start replays the record of
// every enabled plugin and each of those needs a socket.
func (c *Craft) installHostWire() {
	if c.plugins == nil || c.hostWire == nil {
		return
	}
	host, ok := c.plugins.(*plugin.Host)
	if !ok {
		return
	}
	host.SetServer(c.hostWire.server)
}

// closeHostWire tears every plugin connection down. A plugin process that
// outlived its connection leaves nothing behind, because the only thing it
// held was the file descriptor.
func (c *Craft) closeHostWire() error {
	if c.hostWire == nil {
		return nil
	}
	c.hostWire.mu.Lock()
	if c.hostWire.closed {
		c.hostWire.mu.Unlock()
		return nil
	}
	c.hostWire.closed = true
	server := c.hostWire.server
	c.hostWire.mu.Unlock()
	return server.Close()
}

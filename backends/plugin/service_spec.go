package plugin

import (
	"github.com/GizClaw/flowcraft/backends/plugin/service"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// ServiceSpec converts a service artifact into the transport spec the
// RPC channel consumes. The artifact was already shape-validated by
// validateArtifact; this maps the fields the channel needs.
func (a Artifact) ServiceSpec() (service.Spec, error) {
	switch a.Transport {
	case "stdio":
		if a.Command == "" {
			return service.Spec{}, errdefs.Validationf(
				"plugin: service artifact: stdio transport requires command")
		}
		return service.Spec{
			Transport: service.TransportStdio,
			Command:   a.Command,
			Args:      a.Args,
			Env:       a.Env,
		}, nil
	case "http":
		if a.URL == "" {
			return service.Spec{}, errdefs.Validationf(
				"plugin: service artifact: http transport requires url")
		}
		return service.Spec{
			Transport: service.TransportHTTP,
			URL:       a.URL,
			Headers:   a.Headers,
		}, nil
	default:
		return service.Spec{}, errdefs.Validationf(
			"plugin: service artifact: unknown transport %q", a.Transport)
	}
}

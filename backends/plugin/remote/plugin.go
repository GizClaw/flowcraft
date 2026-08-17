package remote

import (
	"context"

	"github.com/GizClaw/flowcraft/backends/plugin"
	"github.com/GizClaw/flowcraft/backends/plugin/service"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// NewPlugin builds a service-backed plugin whose Register installs RPC
// proxy factories for the manifest's service capabilities. The v1
// adapter supports only inference.Provider capabilities.
func NewPlugin(manifest plugin.Manifest, spec service.Spec) (plugin.Plugin, error) {
	var capabilities []resource.Spec
	for _, artifact := range manifest.Artifacts {
		if plugin.ArtifactType(artifact.Type) != plugin.ArtifactService {
			continue
		}
		capabilities = append(capabilities, artifact.Capabilities...)
	}
	if len(capabilities) == 0 {
		return nil, errdefs.Validationf(
			"remote: plugin %s declares no service capabilities", manifest.Name)
	}
	for _, capability := range capabilities {
		if capability.Kind != "inference.Provider" {
			return nil, errdefs.Validationf(
				"remote: plugin %s: capability %s/%s is not supported "+
					"(v1 anchor is inference.Provider)",
				manifest.Name, capability.Kind, capability.Impl)
		}
	}
	return &servicePlugin{
		manifest:     manifest,
		spec:         spec,
		capabilities: capabilities,
	}, nil
}

// servicePlugin is the RPC service slot: it registers proxy factories
// that lazily start the plugin process on first resource construction.
type servicePlugin struct {
	manifest     plugin.Manifest
	spec         service.Spec
	capabilities []resource.Spec

	svc *service.Service
}

func (p *servicePlugin) Manifest() plugin.Manifest { return p.manifest }

func (p *servicePlugin) Layers() []deploy.Layer { return nil }

// Register implements plugin.Plugin: it creates the transport service
// (without starting it) and registers one proxy factory per declared
// capability.
func (p *servicePlugin) Register(ctx context.Context, target *plugin.Target) error {
	svc, err := service.New(p.spec)
	if err != nil {
		return err
	}
	for _, capability := range p.capabilities {
		if err := target.Resources.Register(providerFactory{
			svc:  svc,
			spec: capability,
		}); err != nil {
			return err
		}
	}
	p.svc = svc
	return nil
}

// Close implements plugin.Plugin: it terminates the plugin process,
// which also releases every RPC handle it owned.
func (p *servicePlugin) Close() error {
	if p.svc == nil {
		return nil
	}
	return p.svc.Close(context.Background())
}

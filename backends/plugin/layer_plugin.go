package plugin

import (
	"context"

	"github.com/GizClaw/flowcraft/core/deploy"
)

// layerPlugin is the declaration-slot plugin: it contributes
// deploy.Layer artifacts and registers no factories.
type layerPlugin struct {
	manifest Manifest
	layers   []deploy.Layer
	fp       string
}

func (p *layerPlugin) Manifest() Manifest { return p.manifest }

func (p *layerPlugin) Layers() []deploy.Layer { return p.layers }

func (p *layerPlugin) Register(context.Context, *Target) error { return nil }

func (p *layerPlugin) Close() error { return nil }

func (p *layerPlugin) fingerprint() string { return p.fp }

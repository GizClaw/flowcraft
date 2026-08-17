package plugin

import (
	"context"
	"errors"
	"io"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Plugin is the minimal plugin unit as seen by the loader.
type Plugin interface {
	// Manifest returns the plugin's static description.
	Manifest() Manifest
	// Layers returns the configuration layers contributed by the
	// plugin (declaration slot).
	Layers() []deploy.Layer
	// Register writes the plugin's factories into the target. Layer
	// plugins register nothing; RPC-backed plugins register proxy
	// factories that forward to the service channel.
	Register(ctx context.Context, target *Target) error
	io.Closer
}

// Target is where plugin-registered capabilities land.
type Target struct {
	Resources *resource.Registry
}

// NewTarget returns a target backed by a fresh resource registry.
func NewTarget() *Target {
	return &Target{Resources: resource.NewRegistry()}
}

// Set is one load result: the activated plugins plus the merged layer
// sequence.
type Set struct {
	Plugins []Plugin
	Layers  []deploy.Layer // sorted by Priority ascending
}

// Apply registers every plugin into target in order.
func (s *Set) Apply(ctx context.Context, target *Target) error {
	for _, p := range s.Plugins {
		if err := p.Register(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

// Close closes every plugin in reverse registration order. All plugins
// are closed even when one fails.
func (s *Set) Close() error {
	var errs []error
	for i := len(s.Plugins) - 1; i >= 0; i-- {
		if err := s.Plugins[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

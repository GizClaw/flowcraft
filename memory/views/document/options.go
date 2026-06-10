package document

import "github.com/GizClaw/flowcraft/memory/views"

// Option configures Chunks descriptors.
type Option interface {
	applyChunks(*Chunks)
}

type descriptorOption struct {
	id      views.ID
	version string
}

// WithID overrides the descriptor ID.
func WithID(id views.ID) Option {
	return descriptorOption{id: id}
}

// WithVersion overrides the descriptor version.
func WithVersion(version string) Option {
	return descriptorOption{version: version}
}

func (o descriptorOption) applyChunks(c *Chunks) {
	if o.id != "" {
		c.id = o.id
	}
	if o.version != "" {
		c.version = o.version
	}
}

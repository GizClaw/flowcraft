package indexed

import (
	"maps"
	"strings"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const errPrefix = "memory/internal/views/indexed"

const (
	// MetadataSourceRefsKey stores the source refs used to produce a retrieval
	// document when Record.SourceRefs is set.
	MetadataSourceRefsKey = "indexed.source_refs"
	// MetadataSignatureKey stores the view signature used to produce a retrieval
	// document when Record.Signature is set.
	MetadataSignatureKey = "indexed.signature"
)

// Record is the payload written to a physical retrieval namespace.
type Record struct {
	ID         string
	Text       string
	Vector     []float32
	Metadata   map[string]any
	SourceRefs []views.SourceRef
	Signature  views.ViewSignature
}

// Binding declares the physical retrieval namespace for an indexed projection.
type Binding struct {
	Namespace string
}

// Validate checks that the binding can address a retrieval namespace.
func (b Binding) Validate() error {
	if b.Namespace == "" {
		return errdefs.Validationf("%s: namespace is required", errPrefix)
	}
	if len(b.Namespace) > 48 {
		return errdefs.Validationf("%s: namespace must be 48 characters or fewer", errPrefix)
	}
	for _, ch := range b.Namespace {
		if !validNamespaceChar(ch) {
			return errdefs.Validationf("%s: namespace %q must contain only ASCII letters, digits, and underscores", errPrefix, b.Namespace)
		}
	}
	return nil
}

// Validate checks that a record is specific enough to become a retrieval.Doc.
func (r Record) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return errdefs.Validationf("%s: record id is required", errPrefix)
	}
	if strings.TrimSpace(r.Text) == "" && len(r.Vector) == 0 {
		return errdefs.Validationf("%s: record text or vector is required", errPrefix)
	}
	if _, ok := r.Metadata[MetadataSourceRefsKey]; ok {
		return errdefs.Validationf("%s: metadata key %q is reserved", errPrefix, MetadataSourceRefsKey)
	}
	if _, ok := r.Metadata[MetadataSignatureKey]; ok {
		return errdefs.Validationf("%s: metadata key %q is reserved", errPrefix, MetadataSignatureKey)
	}
	for _, ref := range r.SourceRefs {
		if err := ref.Validate(); err != nil {
			return errdefs.Validationf("%s: invalid source ref: %w", errPrefix, err)
		}
	}
	if err := r.Signature.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid signature: %w", errPrefix, err)
	}
	return nil
}

func cloneBinding(in Binding) Binding {
	return in
}

func validNamespaceChar(ch rune) bool {
	return ch == '_' ||
		(ch >= 'a' && ch <= 'z') ||
		(ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9')
}

func cloneMetadata(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	return maps.Clone(in)
}

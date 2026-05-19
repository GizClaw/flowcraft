package recall

import (
	"context"
	"sort"
	"sync"
)

// NamespaceRegistry tracks namespaces that recall may need to sweep.
//
// Implementations must be safe for concurrent use. Remember is best-effort
// metadata maintenance; callers should not fail core read/write paths if it
// returns an error.
type NamespaceRegistry interface {
	Remember(ctx context.Context, ns string) error
	List(ctx context.Context) ([]string, error)
	Close() error
}

type memNamespaceRegistry struct {
	mu  sync.RWMutex
	set map[string]struct{}
}

// NewMemoryNamespaceRegistry returns an in-memory NamespaceRegistry.
func NewMemoryNamespaceRegistry() NamespaceRegistry {
	return &memNamespaceRegistry{
		set: map[string]struct{}{},
	}
}

func (r *memNamespaceRegistry) Remember(ctx context.Context, ns string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ns == "" {
		return nil
	}
	r.mu.Lock()
	r.set[ns] = struct{}{}
	r.mu.Unlock()
	return nil
}

func (r *memNamespaceRegistry) List(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	out := make([]string, 0, len(r.set))
	for ns := range r.set {
		out = append(out, ns)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out, nil
}

func (*memNamespaceRegistry) Close() error { return nil }

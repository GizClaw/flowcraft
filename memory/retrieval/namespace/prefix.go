package namespace

import (
	"fmt"
	"sync"
)

var (
	registryMu sync.Mutex
	registry   = map[string]struct{}{}
)

// Prefix is a registered namespace owner such as "ltm" or "kb".
type Prefix struct {
	name string
}

// Register reserves a namespace prefix for one subsystem.
func Register(name string) (*Prefix, error) {
	if !IsValidPrefix(name) {
		return nil, fmt.Errorf("retrieval/namespace: invalid prefix %q", name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[name]; ok {
		return nil, fmt.Errorf("retrieval/namespace: prefix %q already registered", name)
	}
	registry[name] = struct{}{}
	return &Prefix{name: name}, nil
}

// MustRegister is Register for package-level namespace owners.
func MustRegister(name string) *Prefix {
	p, err := Register(name)
	if err != nil {
		panic(err)
	}
	return p
}

// MustRegisterAlias is MustRegister for deprecated compatibility packages that
// intentionally share the same namespace owner as their canonical replacement.
//
// New subsystems should use MustRegister so accidental prefix collisions remain
// visible at init time.
func MustRegisterAlias(name string) *Prefix {
	if !IsValidPrefix(name) {
		panic(fmt.Errorf("retrieval/namespace: invalid prefix %q", name))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = struct{}{}
	return &Prefix{name: name}
}

// String returns the raw prefix name.
func (p *Prefix) String() string {
	if p == nil {
		return ""
	}
	return p.name
}

// IsValidPrefix reports whether name is safe as a retrieval namespace prefix.
func IsValidPrefix(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

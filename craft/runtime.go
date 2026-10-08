package craft

import (
	"context"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/core/runtime"
)

// Runtime returns the live runtime registered under key.
func (c *Craft) Runtime(key RuntimeKey) (*runtime.Runtime, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rt, ok := c.runtimes[key]
	return rt, ok
}

// Runtimes returns the sorted runtime keys.
func (c *Craft) Runtimes() []RuntimeKey {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]RuntimeKey, 0, len(c.runtimes))
	for key := range c.runtimes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// bindRuntime runs every RuntimeBinder after a successful build or
// reload.
func (c *Craft) bindRuntime(
	ctx context.Context,
	key RuntimeKey,
	rt *runtime.Runtime,
) error {
	for _, capability := range c.caps {
		binder, ok := capability.(RuntimeBinder)
		if !ok {
			continue
		}
		if err := binder.BindRuntime(ctx, key, rt); err != nil {
			return fmt.Errorf(
				"craft: capability %s bind runtime %q: %w",
				capability.Name(), key, err)
		}
	}
	return nil
}

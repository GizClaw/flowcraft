package projection

import (
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// layoutVersion is the projection storage layout this build writes. It is a
// segment of every key a lane owns, so a build that changes the shape of what
// it stores bumps it instead of teaching every reader two shapes.
const layoutVersion = "v3"

// legacyLayoutVersions names the layouts this build does not read and that an
// earlier build of this module wrote. A layout bump appends the version it
// replaces here: a version that is not listed is never looked at again, which
// is what keeps the list from deleting state a build still serves.
var legacyLayoutVersions = []string{"v2"}

// layoutRoot is one scope's partition under one layout version. The partition
// is the same in every version -- runtime, tenant, agent, encoded -- so a
// sweep of a legacy layout reaches exactly the state the current layout's own
// scope holds, and no other tenant's.
func (store *Store[B, D]) layoutRoot(scope corememory.Scope, version string) string {
	return path.Join("projections", store.Family, version, "partitions",
		encode(scope.RuntimeID), encode(scope.UserID), encode(scope.AgentID))
}

// indexesPrefix is the area every index name of one family and scope lives
// under, the parent of one lane's root.
func (store *Store[B, D]) indexesPrefix(scope corememory.Scope) string {
	return path.Join(store.layoutRoot(scope, layoutVersion), "indexes")
}

// ReclaimLegacyLayouts removes every key one scope holds under a projection
// layout this build does not read, and reports what it removed.
//
// The layout a scope's state lives under is a segment of its path, and this
// build writes exactly one of them. A scope indexed by an earlier build
// therefore holds a tree nothing reads: not a lane, not a manifest, and not
// the sweeps that walk a lane's own root -- Reclaim keeps what a manifest
// names inside the current layout, and OtherStoredNames lists names inside it
// too. The state such a layout holds is not history a later read reaches; it
// is storage no caller can even name, which is what this sweep gives back.
//
// It deletes a subtree rather than the artifacts of a protocol: the object
// types under a layout this build does not read are not ones it can parse, so
// there is nothing to compare against a manifest and nothing to keep. That is
// also why the sweep is scoped to one partition -- a tenant reclaiming its own
// state must not reach the tenant beside it, and the partition is the only
// address both layouts agree on.
//
// A legacy layout is not state a rollback is served from: a build that read it
// would find the current layout unreadable the same way. The workspace is
// rebuilt from canonical state, which is the property the layout bump was made
// on.
func (store *Store[B, D]) ReclaimLegacyLayouts(
	ctx context.Context,
	scope corememory.Scope,
) (component.LegacyReclaimResult, error) {
	var result component.LegacyReclaimResult
	if ctx == nil {
		return result, errors.New("projection: context is required")
	}
	if err := scope.Validate(); err != nil {
		return result, err
	}
	for _, version := range legacyLayoutVersions {
		if version == layoutVersion {
			// Refused before the first delete: a list that named the layout
			// this build writes would sweep the state every read resolves.
			return component.LegacyReclaimResult{}, fmt.Errorf(
				"projection: layout %q is both current and legacy", version)
		}
	}
	for _, version := range legacyLayoutVersions {
		root := store.layoutRoot(scope, version)
		keys, err := indexKeys(ctx, store.KV, root)
		if err != nil {
			return result, fmt.Errorf("projection: list %s layout: %w", version, err)
		}
		if len(keys) == 0 {
			continue
		}
		for _, key := range keys {
			if err := store.KV.Delete(ctx, key); err != nil {
				return result, fmt.Errorf("projection: reclaim %s layout key %q: %w", version, key, err)
			}
		}
		result.Layouts = append(result.Layouts, component.LegacyLayout{Version: version, Keys: len(keys)})
		result.Keys += len(keys)
	}
	return result, nil
}

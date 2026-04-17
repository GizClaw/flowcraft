package realm

import "context"

// RealmProvider is the only supported way to obtain a [Realm] for execution.
// The default implementation is [SingleRealmProvider] (single owner realm).
type RealmProvider interface {
	// Resolve returns the realm for this process, creating it on first use.
	Resolve(ctx context.Context) (*Realm, error)
	// Current returns the realm if already created, without initializing.
	Current() (*Realm, bool)
	// Stats returns a snapshot of the provider's state.
	Stats() RealmProviderStats
	Close()
}

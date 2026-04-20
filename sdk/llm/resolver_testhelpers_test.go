package llm

// newResolverWithRegistry is the test-only constructor that lets us
// inject a custom ProviderRegistry while keeping the rest of the
// resolver's setup identical to the public DefaultResolver — i.e. all
// store-interface dispatch, caching, and option wiring are exercised
// exactly as production code would. Tests should NOT construct
// &defaultResolver{...} directly; do everything through this helper
// plus DefaultResolver-style options.
func newResolverWithRegistry(store ProviderConfigStore, reg *ProviderRegistry, opts ...ResolverOption) LLMResolver {
	r := DefaultResolver(store, opts...).(*defaultResolver)
	r.registry = reg
	return r
}

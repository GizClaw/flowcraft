// Package yaml is the versioned YAML loader for the memory
// configuration and the deploy.ResourceFactory that turns a
// memory.yaml on disk into a *config.Assembly.
//
// The package is intentionally small: a thin wrapper that
// decodes a memory.yaml into a config.Document, then calls
// the supplied [config.Builder] to assemble the runtime. All
// validation lives in [config.Document.Validate]; all
// store-specific work lives in the registered StoreFactory
// catalog.
package yaml

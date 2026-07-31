// Package config loads versioned YAML declarations of named workspace
// resources. It is intentionally independent from sdkx/agent/config: build a
// workspace Registry here, then expose selected entries to agent config through
// an application-owned source.
//
// A complete document looks like:
//
//	version: v1
//	workspaces:
//	  project:
//	    driver: local
//	    settings:
//	      root: ./workspace
//	    scope:
//	      deny_read: ["**/.env"]
//	      allow_write: ["**"]
//	      mandatory_deny: [".git/**"]
//	  scratch:
//	    driver: memory
//
// Relative local roots resolve against Builder Deps.BaseDir. The built Registry
// retains host-root metadata separately from the Workspace interface so a
// sandbox config can reuse the same root without duplicating paths in YAML.
// Registry.Resolve is directly assignable to sdkx/agent/config.SourceFunc.
// Applications must close the Registry when finished so custom closeable
// workspace drivers can release their resources.
package config

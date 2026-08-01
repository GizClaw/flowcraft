// Package config loads versioned YAML declarations of named workspace
// resources. The loader itself knows nothing about agents; [DeployResource]
// is the opt-in adapter that lets a sdkx/deploy document own a Registry, and
// [Registry.Resolve] covers the opposite case where the host owns it.
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
// Whoever builds the Registry closes it: sdkx/deploy does so for a Registry
// declared in its resource area, and the application does so for one it built
// and exposed through [Registry.Resolve].
package config

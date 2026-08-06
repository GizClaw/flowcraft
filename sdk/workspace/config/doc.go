// Package config loads versioned declarations of named workspace
// resources. The loader itself knows nothing about agents;
// [NewDeployFactory] is the opt-in adapter that lets a deployment
// document own a Registry, and [Registry.Resolve] covers the opposite
// case where the host owns it.
//
// The document is JSON at the protocol level; YAML is accepted as
// authoring sugar through sdk/config/utils, which detects JSON by the
// Kubernetes rule (first non-whitespace byte is an open brace) and
// converts YAML before strict decoding:
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
// Relative local roots resolve against Builder Deps.BaseDir. The built
// Registry retains host-root metadata separately from the Workspace
// interface so a sandbox config can reuse the same root without
// duplicating paths in the document. Whoever builds the Registry closes
// it: a deployment engine does so for a Registry declared in its
// resource area, and the application does so for one it built and
// exposed through [Registry.Resolve].
package config

// Package config parses and builds independently named sandbox
// resources.
//
// Sandbox configuration is intentionally separate from agent
// configuration: applications build this package's Registry first, then
// inject selected runners into agent configuration as an
// application-owned config source.
//
// The framework follows sdk/workspace/config: the builder owns a
// backend factory catalog, the local backend is built in, and platform
// backends (seatbelt, nsjail, ...) register themselves from their own
// packages with their own settings.
//
// The document is JSON at the protocol level; YAML is accepted as
// authoring sugar through sdk/config/utils:
//
//	version: v1
//	sandboxes:
//	  coding:
//	    backend: seatbelt
//	    workspace: project
//	    settings:
//	      writable_paths: [".cache"]
//	    defaults:
//	      timeout: 2m
//	      env:
//	        allow: [PATH, HOME]
//	      net:
//	        mode: deny_all
//	      resources:
//	        memory_bytes: 1073741824
//	        max_output_bytes: 1048576
//	    allowed_commands: [go, git]
//	    approval:
//	      outside_workdir: true
//	      non_default_network: true
//	      sensitive_commands: [rm, git]
//
// Built-in backend settings paths are consistent: absolute paths remain
// absolute, while relative binary and writable_paths values are resolved
// beneath the referenced workspace root.
//
// Builder validates declared defaults against the selected backend's
// Enforcement report, so unsupported policy combinations fail during
// assembly instead of on the first command.
//
// Whoever builds the Registry closes it: a deployment engine does so
// for a Registry declared in its resource area via [NewDeployFactory],
// and the application does so for one it built and exposed through
// [Registry.Resolve].
package config

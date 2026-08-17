// Package plugin implements the FlowCraft plugin shell: discovery,
// validation, dependency and conflict checks, and declaration-layer
// assembly for third-party plugin directories.
//
// A plugin is a directory with a strictly decoded plugin.yaml
// manifest. The shell depends only on the core protocol packages
// (resource, deploy, errdefs) and knows nothing about concrete
// resource kinds; RPC-backed implementations of host-defined resource
// contracts arrive in a later phase.
package plugin

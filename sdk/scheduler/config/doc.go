// Package config exposes scheduler servers as deployment resources.
//
// The framework follows sdk/sandbox/config: the Builder owns a server
// implementation catalog, and concrete implementations (such as the
// process-local server in sdkx/scheduler) register themselves with
// their own settings. [NewDeployFactory] wraps one implementation name
// so a deployment document can select it by impl.
package config

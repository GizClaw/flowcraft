// Package config wires memory implementation factories into deployment
// resources. It knows only the sdk/memory contracts; every concrete
// implementation (such as the flowcraft memory module) lives in its own
// module and is registered here by the application, exactly like
// inference provider factories.
package config

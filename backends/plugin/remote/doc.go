// Package remote adapts host-defined resource contracts to the plugin
// RPC channel. Each adapter implements the corresponding Go interface
// and forwards its methods as resource.call invocations on the
// service.
//
// The v1 anchor is inference.Provider/rpc: a provider plugin answers
// unary generate over resource.call and, when it declares streaming on
// an http transport, incremental generate_stream over a separate SSE
// /stream endpoint.
package remote

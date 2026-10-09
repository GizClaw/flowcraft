// Package inferencetest provides reusable conformance suites for inference
// providers.
//
// [ScriptedOpenAI] is the wire-level counterpart of those suites: a
// scriptable, OpenAI-compatible chat completions server that speaks the real
// HTTP format (JSON and SSE), for tests that need the actual transport,
// retry, and stream-decoding paths instead of an in-process fake. The suites
// check the shared Runtime contracts; the scripted server checks the wire.
//
//	srv := inferencetest.NewScriptedOpenAI(t,
//		inferencetest.ScriptedReply{Text: "hello"},
//	)
//	// Point the driver under test at srv.URL() (the "/v1" base), call it,
//	// then assert what the provider saw and what came back:
//	if got := srv.Calls(); got != 1 {
//		t.Fatalf("provider saw %d calls, want 1", got)
//	}
//	messages, err := srv.LastMessages()
//
// Provider packages should keep provider-specific wire assertions in their own
// tests and use these suites for the shared Runtime contracts: Explain must
// not perform provider I/O, execution metadata must retain compiler
// decisions, requests must remain caller-owned, generate unary/stream paths
// must agree on their active field set, stream failures must surface through
// Next/Result/Close, and compilers/transports/decoders must be safe for
// concurrent use.
package inferencetest

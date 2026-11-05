// Package protocol is the driver author's kit: the three stages a provider call
// is made of, the binder that turns them into an [inference.Provider], and the
// contract check that makes a dropped option impossible to hide.
//
// # Why this is a separate package
//
// Hosts import inference and never import this package. Drivers import both: the
// vocabulary from inference, the pipeline from here. That split is what lets the
// pipeline keep its internal steps unexported — a driver hands over three
// functions and receives a working provider, without ever seeing how a prepared
// request is threaded through execution.
//
// # The three stages
//
//	Compiler   canonical request -> provider-native request      no I/O
//	Transport  provider-native request -> raw provider answer    the only I/O
//	Decoder    raw provider answer -> canonical response         no I/O
//
// Splitting them this way is not decoration: it is what lets a driver be tested
// without a network. A conformance suite drives the Compiler against expected
// wire values, and a fake Transport exercises the rest, so a mapping bug is
// caught in a unit test instead of in a live call.
//
// # The contract check
//
// A [Compiled] value carries the fields the compilation honored and the warnings
// it raised. After every compilation the pipeline verifies two things: no field
// was honored that the request did not set, and every field the request did set
// is either honored or explained by a warning. A field that falls through
// neither is a driver bug and surfaces as
// [inference.CompilerContractViolation] — the option is never silently lost.
package protocol

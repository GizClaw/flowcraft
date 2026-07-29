// Package inference is the instance-owned runtime for unified model inference.
//
// Four root operations cover every supported workload: Generate (unary and
// finite streaming text/image/audio/tool calls), Embed, Transcription (unary
// and streaming sessions), and Realtime (bidirectional sessions). Callers
// address an exact ModelRef — provider, model name, and credential profile —
// and the Runtime resolves, compiles, and executes against it.
//
// The provider compiler is the sole authority on whether a concrete request
// is executable. Canonical requests declare their active fields through a
// field ledger; compilers must account for every active field with a Native
// or Rejected decision, and the pipeline enforces that ledger on both success
// and failure. Anything the compiler cannot honor natively is a structured
// rejection (UnsupportedFeature/InvalidExtension), never a silent drop.
//
// Profiles scope credentials to operations: a provider may expose several
// API keys as named profiles, each restricted to the operations its backing
// credentials can execute. Selectors and deployment routing live in the
// route subpackage; versioned deployment configuration, secret resolution,
// and hot reload live in the separate sdkx/inference/config module. This
// package deliberately contains no I/O beyond provider transport, no global
// registries, and no deployment policy of its own.
package inference

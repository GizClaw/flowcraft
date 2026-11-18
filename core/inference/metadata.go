package inference

import "github.com/GizClaw/flowcraft/core/inference/outcome"

// Metadata describes a completed call: the resolved model and the provider's own
// request and response ids.
//
// It is declared in core/inference/outcome, where it sits beside the accounting
// of the same call. That package is a leaf: a durable record, an event or a
// reader of either can name what answered without importing this contract —
// provider interfaces, requests, streaming, the error vocabulary.
//
// This alias keeps the spelling `inference.Metadata` compiling for one release
// cycle, because the driver module is built against two core versions at once.
// It goes away with the release that moves the driver to `outcome`; nothing new
// belongs behind it.
type Metadata = outcome.Metadata

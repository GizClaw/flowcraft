// Package projection decides what a session's model may see, and how it finds what it
// cannot.
//
// # What it is not
//
// A projection is not a gate. It answers "what should be shown this round", and a model
// can still name a tool that was never shown: authority is dispatch's question, and the
// two are kept apart on purpose so visibility can change without changing what is legal.
//
// It is also not a session store. Session state — which tools this conversation found,
// which it used last round — lives in a [View] the host keeps next to the session it
// serves. Projection holds only what every session shares: the catalog, the exposure
// policy and the search index.
//
// # The three states
//
// A tool is visible for one of three reasons, in the order the policy ranks them:
//
//   - [ExposureAlways]: shown every round, the stable baseline.
//   - required: the agent asked for it by name ([View.Require]).
//   - discovered: the model found it through tool_search or used it recently.
//
// A [ExposureHidden] tool is visible only when required. Everything else is
// [ExposureDeferred] by default: the model starts with a small toolset and discovers the
// rest, which keeps a large catalog from spending the round's context on definitions the
// task does not need.
//
// # Two budgets
//
// The per-round [Budget] caps what reaches the model this turn. The [DiscoveryPolicy]
// caps what the session remembers between rounds — the tools the model has already found
// stay loaded while they are used and expire after idle rounds. They are separate
// numbers because they answer separate questions: "how much context may one turn spend"
// and "how much may a session keep warm".
//
// # tool_search
//
// The discovery path is itself a tool ([SearchTool]). It ranks the catalog with BM25,
// loads each hit so the next round shows the real definition rather than a placeholder,
// and adds the hits to the session's pool. It reads the session's [View] out of the
// binding, so a session can only discover what its own projection may show.
package projection

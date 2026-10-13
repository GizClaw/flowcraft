// Package typesafe implements the TypeSafe System One provider for the unified
// inference runtime. It owns strict Spec decoding, the model line-up a
// deployment declares, and all request lowering; core/inference never sees
// System One concepts.
//
// Operation coverage:
//
//   - Decision: the System One route, POST {base_url}/systemone with Bearer
//     auth. One state value plus a map of typed questions, answered in one
//     unary response. The canonical question kinds map onto the vendor's
//     three: predicate onto "noul" (the vendor's word for a yes/no
//     proposition, which appears nowhere outside this driver), choice onto
//     "choice", and score onto "score". There is no refusal on this wire, so
//     a response never replaces an answer; a kind outside the three is a
//     contract violation rather than a value to coerce.
//
// Nothing else is served, and nothing else is planned here: the vendor's
// endpoint is one route with one request shape, so there is no streaming form
// to bind, no session surface to own, and no second model family to dispatch
// on. The driver also ships no model line-up — a declaration is the whole fact
// about a model — and the vendor's model-listing route (GET /v1/models) is
// therefore unused.
//
// # State carriers
//
// A decision request carries exactly one evidence value, and every question is
// evaluated against it. The canonical text carrier lowers to a JSON string and
// the canonical json carrier lowers to the raw object or array: this is the one
// place the System One wire is wider than its OpenAI sibling, which has no
// structured field and flattens the same evidence to its serialized text. The
// parts carrier is refused through the ledger, per part, rather than flattened:
// the wire has one state value and no message part concept, so text parts would
// silently become a different state than the caller wrote, and images are not a
// System One concept at all — the vendor's answer to non-text material is to
// pre-process it into text or a structured state field before sending it.
//
// # Question and answer shapes
//
// A question's canonical name is the wire's map key. The key is the caller's
// locator and is never sent to the model, which is exactly the canonical
// contract: the strictest locator both wires can express, since the OpenAI
// sibling's answers are placed by position and its name field is optional.
//
// Two declarations are refused rather than approximated:
//
//   - A boolean choice option. The wire's criteria is a map from option to
//     rubric description, so an option's text is its key: the boolean true and
//     the string "true" would collapse into one option, losing the typed
//     distinction the canonical layer keeps. The option is rejected instead.
//   - A score level description. The wire's criteria carries one string per
//     level — the level's own label — so a second description has no channel;
//     sending only the label would let a compile report claim the model
//     received a rubric it never saw.
//
// A noul never sends criteria: the canonical layer carries no yes/no criteria
// field, so there is nothing to lower (the wire treats criteria as optional).
// The wire's structured instructions form (an object or array) is likewise
// unused: the canonical layer carries a string, and the driver sends it as one.
//
// The response is read strictly. The answers arrive as a map keyed by the
// caller's ids while the canonical response is an ordered list reconciled
// against the questions by position, and the ids never reach the model — so the
// transport folds the map onto the request order, refusing a question the
// response did not answer and an id the request never asked. Every answer then
// has to be complete and self-consistent: a noul carries a probability (there
// is no separate confidence on this wire — one number describes a two-outcome
// distribution completely), a choice carries its chosen option, a complete
// distribution, and a confidence, and a score carries a value, a legend and a
// distribution that name the same levels, and a confidence. A choice
// distribution must name only offered options and sum to one; a score legend
// must parse to distinct level indexes and match its distribution key for key.
// The wire reports input and output tokens and no total, so the canonical total
// is their sum. The reported model id is the versioned id that answered
// (jev-1.13.0) rather than the alias that was requested (jev-latest).
//
// The driver enforces the route's numeric bounds at compile time — 255 options
// per choice question and 10 levels per score question, both published — because
// the canonical layer deliberately owns only the two-option minimum, which both
// wires agree on. The question count is the driver's own ceiling of 128: the
// vendor publishes none, so the limit is a policy in this package rather than a
// vendor fact. The published context budget (64k tokens for the state plus all
// questions, 32k for the state plus the single longest question) is a capacity
// of the model rather than a route limit, so it is not enforced here; the
// model's own limits filter routing.
//
// # Credentials, transport, and retries
//
// Credentials come exclusively from config profiles: `api_key` authenticates
// the route. The provider Spec redirects transport (`endpoint.base_url`, which
// includes the version segment and defaults to
// https://api.typesafe.ai/v1), adds static `endpoint.headers` for gateways in
// front of the route (the two headers the transport owns — Authorization and
// Content-Type — are refused there), and declares the models this deployment
// serves (`models`). The profile Spec is reserved and currently carries no
// settings; an unknown key fails the build rather than being ignored.
//
// `http_retries` is the total number of wire attempts including the first. Nil
// keeps the httpkit default (three attempts); 0 disables transport retries so
// the route Router owns the whole budget. The vendor's two transient statuses
// — 429 and 529 — are graded retryable by the shared status mapping, and
// Retry-After and the wire attempt count are propagated onto ProviderFailure so
// Router backoff and trace wire_attempts can observe them. The vendor publishes
// 401, 422, 429, and 529 and no request-id header; the driver reads the
// conventional x-request-id when a gateway in front of the route sets one, so a
// failure still correlates without inventing an envelope.
//
// Deployments wire this package as an inference.Provider resource:
//
//	reg.MustRegister(typesafe.Factory())
//
// and reference it from the inference assembly:
//
//	resources:
//	  provider.typesafe: {kind: inference.Provider, impl: typesafe, settings: {...}}
//	  infer: {kind: inference.Assembly, impl: unified, deps: {provider.typesafe: provider.typesafe}}
//
// This driver ships no provider extension: a request carrying one is refused
// field by field through the ledger, because every extension that reaches this
// provider is meant for another operation or another deployment and silently
// ignoring it would report a request the caller never configured.
package typesafe

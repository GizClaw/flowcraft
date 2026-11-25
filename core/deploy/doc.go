// Package deploy turns deployment documents into values — and nothing else.
//
// # Two documents
//
// A process document says what this process carries: provider targets, sandbox backends,
// tool sources, skill roots, and how much of a turn it records. An agent document says what one
// agent is: its prompt, its program, its grant and its hooks.
//
// Neither document can name a resource the other one owns. An agent selects from the
// process's resources through its [policy.Grant]; it cannot add a provider or a backend. To
// add resources you change the process document, which means restarting the process — a
// different action from deploying an agent, and the error messages keep the two apart.
//
// # Values, not handles
//
// [Loader.Process] returns a [ProcessPlan]: for every entry, the decoded settings and the
// factory that will build it — never the built object. Whoever builds is [host], which is
// also the only layer that closes what it built. The reason is not ceremony: a loader that
// returned handles would own their lifecycle, and "only the assembly layer holds the
// process" would stop being true the moment a test called Load.
//
// The factories come from the application, through [Impls]. Core cannot carry a standard
// table: the shipped implementations live in the sandbox and inference modules, and core is
// forbidden from importing them.
//
// # Two kinds of failure
//
// A document that does not hold together is a [errdefs.Validation] error: unknown field,
// wrong type, missing required value, a kind nobody registered — including an unregistered hook or
// program, because the name is the document's own claim and no process can honour a name it has
// never heard of. A document that holds together but asks for something this process does not have
// is [errdefs.NotAvailable]: a sandbox backend that is not configured here, a model that is not
// wired, a middleware link the process filtered out. The first is fixed by editing the document,
// the second by changing how the process was started.
package deploy

// Package craft assembles and owns a FlowCraft application: the
// craft.yaml definition, its shared services, and the keyed set of
// runtimes built from deployment documents.
//
// A Craft owns shared services (configuration resolution, the event
// plane, plugin host, host primitives, UI registry) and one or more
// runtimes keyed by an application-defined RuntimeKey (a workspace, a
// profile, ...). It does not own the process lifecycle; that is the
// optional craft/manager package (definition lookup, profiles, locks,
// migrations, supervision, signal handling).
package craft

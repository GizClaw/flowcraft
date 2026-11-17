// Package contextkeys is the module-private gate over the values core carries on
// context.Context.
//
// It has no API and no non-test code: its one job is to keep
// docs/guides/context.md and the source in step, so the guide cannot describe a
// value that is gone and a new value cannot appear without a row. It sits in its
// own package because it reads files across the whole module — no single package
// owns the set — and because nothing outside core may import it.
package contextkeys

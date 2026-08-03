// Package hook wires the *memory.Runtime surface into the
// agent lifecycle through deploy-registered factories.
//
// Each factory in this package reads its typed settings off
// the deploy document, resolves the *memory.Runtime from the
// declared dep, and returns an agent.Preparer or
// agent.Committer that calls the corresponding op on every
// Run. Settings are decoded strictly with
// [deploy.DecodeSettings] so a typo in the document fails
// build, not run.
//
// Recall queries use a strict QuerySpec: Literal passes text unchanged, while
// Board reads a string from the Board produced by the default seeder or an
// earlier Preparer. Request.Inputs are never read directly by the hook.
//
// # Mapping
//
//   - memory.load    → agent.Preparer   (LoadPreparerFactory)
//   - memory.recall  → agent.Preparer   (RecallPreparerFactory)
//   - memory.append  → agent.Committer  (AppendCommitterFactory)
//
// memory.import / memory.compact / memory.archive are
// intentionally absent: Import is a tool (sdkx/memory/tool),
// Compact/Archive are scheduled maintenance tasks
// (sdkx/scheduler/memory). See the memory guide for the
// rationale.
package hook

// Package skill discovers, parses and ranks Agent Skills — directories
// whose SKILL.md carries YAML frontmatter (agentskills.io) — for
// per-turn injection and on-demand reads by an agent loop.
//
// The engine is read-only and policy-free. Roots are parameters: the
// caller composes them (a personal root like ~/.agents/skills, a
// product or plugin directory); nothing is scanned implicitly. Parse
// failures and tolerated shape issues land in [Outcome.Diagnostics];
// discovery never fails hard. Ranking runs over the shared
// [github.com/GizClaw/flowcraft/core/utils/bm25] kernel, so skill
// names and descriptions are scored exactly like tool catalogs.
//
// Lifecycle policies stay with the caller: installation, authoring,
// usage accounting, retirement and built-in shadowing are layered over
// [Service.List], [Service.ByName] and [Service.ReadFull]. Duplicate
// names across roots are all listed and rankable; [Service.ByName]
// resolves the first path in the canonical (name, path) order.
//
// A [Service] holds an immutable snapshot swapped by [Service.Reload],
// so the hot read paths (per-turn ranking, mention resolution,
// rendering) are lock-free and safe for concurrent use.
package skill

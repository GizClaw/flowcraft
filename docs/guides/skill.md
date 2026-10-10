---
layout: default
title: Agent Skills
---
# Agent Skills Guide

`core/skill` implements the read-only half of the Agent Skills
convention (agentskills.io): discovery and parsing of `SKILL.md`
directories, a BM25 index over skill names and descriptions, `$name`
mention resolution, and the per-turn `## Skills` metadata section.
Bodies load on demand.

```go
svc := skill.NewService(ctx, skill.Options{Roots: []string{
    filepath.Join(home, ".agents", "skills"),
    filepath.Join(userDir, "skills"),
}})

// Per turn: explicit $mentions first, then the ranking over the
// request text (an application merges the two lists, de-duplicating
// by path).
for _, sk := range svc.Mentioned(requestText) { ... }
ranked := svc.Rank(requestText, svc.TopN(), svc.MinScore())
section := skill.RenderSection(ranked)

// On demand, when the model opens a skill:
sk, body, err := svc.ReadFull("flowcraft-config")
```

The package owns no policy. Installation, authoring, usage
accounting, retirement, and built-in shadowing are application
concerns layered over `List`, `ByName`, and `ReadFull`: the caller
composes the roots and decides what wins when two roots offer the
same name.

## The SKILL.md contract

A skill is a directory whose `SKILL.md` carries YAML frontmatter:

```markdown
---
name: release-notes
description: Draft release notes from a merged PR list. Use when preparing a release.
---

# Release notes
...
```

| Field | Rule |
| --- | --- |
| `name` | lowercase letters, digits and hyphens, ≤ 64 characters. Missing or invalid names fall back to the slugified directory name, with a warning. |
| `description` | required; truncated to 1024 characters (runes, not bytes). |
| `metadata.short-description` | optional non-standard extension, surfaced for compact listings. |
| file size | ≤ 256 KiB; larger files are refused by discovery and by the body readers. |
| other fields | tolerated: the frontmatter decode is lenient, so vendor extensions (`license`, `version`, …) do not fail the parse. |

The standard expects `name` to equal the directory name; a mismatch
is accepted with a warning. Only malformed files fail the parse
(missing frontmatter, empty body, missing description).

## Roots and discovery

`Discover(ctx, roots)` — and `NewService` underneath it — BFS-walks
every root:

- roots are parameters: the caller composes the conventional personal
  root (`~/.agents/skills`), a product directory, and
  plugin-contributed roots; nothing is scanned implicitly;
- a root may be one skill directory or a tree of them, and a root
  that does not exist is silently empty;
- hidden entries are skipped;
- symlinks are followed only while their target stays inside one of
  the roots, and a skill reachable through several roots (or through
  a symlinked directory) is collected once, under its canonical path;
- parse failures never fail the pass: they land in `Diagnostics`,
  where `Warning` marks a tolerated shape issue and distinguishes it
  from a file that could not load.

Results are sorted by name, then path. Duplicate names across roots
are all kept and all rankable; `ByName` and `Mentioned` resolve the
first path in that canonical order, and shadowing rules (a built-in
losing to a same-named user skill, say) are the caller's to apply.

## The Service

A `Service` holds an immutable snapshot of the last discovery plus
the BM25 index over it. `Reload` re-runs discovery and swaps the
snapshot, so skills added on disk become visible without rebuilding
the service; the read paths are lock-free and safe for concurrent
use.

| Method | Purpose |
| --- | --- |
| `List()` | every discovered skill, sorted by name then path |
| `ByName(name)` | resolve one name to the canonical first path |
| `Diagnostics()` | the non-fatal issues of the last discovery |
| `Roots()` | configured roots that contained at least one skill |
| `TopN()` / `MinScore()` | the configured ranking defaults |

`Options.Disabled` excludes skill names or absolute `SKILL.md` paths
before the index is built, so a disabled skill cannot rank back in.

## Ranking

Ranking runs over the shared `core/utils/bm25` kernel — the same one
behind tool discovery — with the standard Okapi parameters
(k1 = 1.2, b = 0.75): the name field is weighted 3× the description,
a query term that only prefixes an index term is discounted ×0.5,
and CJK text is tokenized into single characters and bigrams, so a
Chinese fragment matches a Chinese description without a
segmentation library.

- `RankScored(query, topN, minScore)` returns skills with their
  scores, best first; `Rank` is the same list without scores. A
  `topN <= 0` falls back to `Options.TopN` (default 5); a
  `minScore <= 0` accepts any match.
- An empty query — or one that matches nothing — returns nil:
  without a signal, nothing is injected.
- `Mentioned(text)` extracts standalone `$name` mentions (`$50` and
  mid-word dollars are not mentions) and resolves them in mention
  order.

## Reads

`ReadFull(name)` and `ReadByPath(path)` return the metadata plus the
body with the frontmatter stripped. `ReadByPath` only serves paths
from the current snapshot, so a stale path from a UI cannot read an
arbitrary file; both re-resolve the file and refuse it when the
symlink target has left the configured roots since discovery.

## Rendering and tool pairing

`RenderSection(skills)` renders the per-turn block:

```text
## Skills
Skills relevant to this turn. To use one, search with skill_search and load its full instructions with skill_read. The user can also activate a skill by mentioning $name.
- release-notes: Draft release notes from a merged PR list. (file: /skills/release-notes/SKILL.md)
```

Bodies are never inlined. The header names `skill_search` and
`skill_read` by convention: the application ships those tools on top
of `RankScored` and `ReadFull` (registered on `core/tool` with the
exposure it needs), so the model always has a discovery path and a
body loader without paying for every skill body each turn.

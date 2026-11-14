---
layout: default
title: Agent Skills
---
# Agent Skills Guide

`core/skill` is the read-only half of the Agent Skills convention
(agentskills.io): it discovers `SKILL.md` directories, parses their frontmatter,
and ranks them with the same BM25 kernel the tool catalog uses. Installing,
authoring, usage accounting and per-turn injection are the host's decisions and
stay out of the package.

The whole surface is three calls: discover, index, ask the index.

```go
outcome := skill.Discover(ctx, tree, []string{".agents/skills", userSkills})
index, err := skill.NewIndex(outcome.Skills)   // one index per discovery pass

ranked := index.Rank(query, 5, 0)              // []Metadata, best first
meta, ok := index.ByName("release-notes")
```

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
| `name` | lowercase letters, digits and hyphens, ≤ 64 characters. A missing or invalid name falls back to the slugified directory name, with a warning; a directory name with no ASCII left to slugify is a parse error. |
| `description` | required; truncated to 1024 characters (runes, not bytes). |
| `metadata.short-description` | optional non-standard extension, surfaced for compact listings. |
| file size | ≤ 256 KiB; larger files are refused. |
| other fields | tolerated: the frontmatter decode is lenient, so vendor extensions (`license`, `version`, …) do not fail the parse. |
| frontmatter keys | two distinct keys spelling one JSON key (`1` beside `"1"`) load with a warning naming the collision; a key the conversion cannot spell refuses the parse, named rather than left to map order. |
| encoding | a UTF-8 BOM before the frontmatter and CRLF line endings are accepted. |

The standard expects `name` to equal the directory name; a mismatch is accepted
with a warning. Only malformed files fail: missing frontmatter, an empty body, a
missing description, an unspellable key.

`Parse(path, data)` is the per-file form, for a host that reads the file itself.

## Discovery

```go
type Outcome struct {
    Skills      []Metadata   // sorted by name, then path
    Diagnostics []Diagnostic // what was tolerated or skipped
    Roots       []string     // the roots that contributed at least one skill
}
```

`Discover(ctx, tree *fsx.Tree, roots []string)` walks every root through `fsx`,
so what a skill tree may contain is the caller's policy, not this package's:

- roots are parameters — the conventional personal root, a product directory,
  plugin-contributed roots — and nothing is scanned implicitly;
- a root may be one skill directory or a tree of them, and a root that does not
  exist is silently empty: a deployment names roots that are not on every
  machine;
- hidden entries are skipped, so `.git` and friends are never skills;
- a skill reachable through several roots is collected once, under its canonical
  path, and walked directories are tracked the same way, so an alias pointing
  back into the tree is skipped instead of re-descended;
- a failure never fails the pass: parse errors and read failures land in
  `Diagnostics`, because one broken third-party skill must not hide the rest.
  `Warning` separates a tolerated shape issue from a file that could not load.

Duplicate names across roots are all kept and all rankable. `ByName` resolves the
first path in canonical order, and shadowing rules — a built-in losing to a
same-named user skill, say — are the host's to apply.

## The index

`NewIndex(skills []Metadata)` builds an immutable index over one discovery pass.
A host that reloads skills builds a new index rather than mutating the old one,
which is what keeps every read lock-free; `core/runtime` keeps exactly one per
runtime and hands it to both the tools and `Runtime.Skills()`.

| Method | Purpose |
| --- | --- |
| `Len()` | how many skills the pass found |
| `All()` | every skill, in discovery order (a stable list, not a map) |
| `ByName(name)` | one skill and whether it exists |
| `RankScored(query, topN, minScore)` | skills with their scores, best first |
| `Rank(query, topN, minScore)` | the same list without scores |

## Ranking

Ranking runs over the shared `core/internal/search` kernel with the standard
Okapi parameters (`k1 = 1.2`, `b = 0.75`): the name field is weighted 3× the
description, a query term that only prefixes an index term is discounted ×0.5,
and CJK text is tokenized into single characters plus adjacent bigrams, so a
Chinese fragment matches a Chinese description without a segmentation library.

`topN <= 0` asks for five; `minScore <= 0` accepts any match. An empty query — or
one that matches nothing — returns nil: without a signal, nothing is offered.

## Reading a body

The package never inlines a skill body. `Metadata.Path` is the absolute path the
skill was discovered at, and the body is read through the same `fsx.Tree` the
discovery used, so the read carries the tree's policy:

```go
body, err := tree.Read(ctx, meta.Path)   // frontmatter included
```

## The tool pairing

`core/tool-builtin/skills` ships the two actions a model can take on the catalog,
over the index the host already built:

| Tool | Use |
| --- | --- |
| `skill_search` | rank the catalog against a query, returning names and descriptions |
| `skill_read` | load one skill's full `SKILL.md` by name |

The index is a parameter rather than something the tools build, which is what
makes "what the model can search" and "what the host reports through
`Runtime.Skills()`" the same catalog instead of two that drift.

## What is not here

| Not in `core/skill` | Where it belongs |
| --- | --- |
| Installing, updating or removing skills | The host, or whatever package manager it trusts. |
| Authoring rules and linting beyond the frontmatter contract | The skill's own tooling. |
| Usage accounting and retirement | The host's telemetry. |
| The per-turn `## Skills` block | The host composes it from `Rank` (and from whatever mention syntax it wants); the package renders no prompt text. |
| Mention resolution (`$name`) | Removed: the tools are the discovery path, and a mention syntax is a prompt decision, not a catalog one. |

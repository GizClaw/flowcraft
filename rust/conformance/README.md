# Conformance gate

The Go implementation is the oracle. `run.sh` builds both sides, runs them
over the corpus, and diffs the two dumps; any mismatch fails the run.

```sh
rust/conformance/run.sh              # the gate (defaults to the adopted backend)
make conformance                     # the same thing, from the repo root
rust/conformance/run.sh --details 40 # more mismatch detail
CONFORMANCE_BACKEND=norway rust/conformance/run.sh   # a rejected candidate
```

`CONFORMANCE_BACKEND` takes `saphyr-go` (the default), `saphyr`, `norway` or
`ng`; the last two build the probe's non-default `candidates` feature, which
the manifest explains.

`saphyr-go` is not a reading the probe implements: it is `flowcraft-utils`,
the crate the port ships, so the gate compares the port against the oracle
through this binary rather than through a copy of it.

| Path | Role |
| --- | --- |
| `fixtures/` | hand-written corner cases: scalars, int/float forms, keys, anchors, merge keys, duplicate keys, alias bombs, tag handling, BOM/CRLF/UTF-8, JSON-vs-YAML sniffing |
| `tools/diff.py` | compares the dumps, per (file, backend) |
| `tools/errortext.py` | compares the *words* of the two sides' refusals, per file |
| `tools/stability.py` | checks the oracle reproduces itself, that every collapsing document is refused, and that every exemption is still true |
| `tools/allowlist.py` | shared reader for `known-divergences.txt` |
| `tools/selftest.py` | negative self-tests: each checker's failure modes, exercised against a broken input |
| `known-divergences.txt` | every accepted disagreement -- a value, a side refusing where the other converts, or wording -- with a reason |
| `out/` | generated dumps and the oracle binary (git-ignored) |

Corpus: `fixtures/`, `examples/`, `skills/`, `docs/guides/`, `docs/migrations/`,
`.release/`, `.github/` — 195 files today, about three seconds end to end
(25 oracle runs dominate). Every listed directory is committed, and that is
the rule: a directory that only exists in a local checkout would make CI and
local runs disagree about which files even exist.

## The backends it measured and rejected

The port did not start from a favourite. Four readings ran into the same dump
before one was adopted, and the other three are still runnable here
(`CONFORMANCE_BACKEND=<name>`), so the choice stays reproducible rather than
remembered.

| Backend | Reading | Why it is not the one |
| --- | --- | --- |
| `norway` | `serde_norway` 0.9.42 straight into a JSON value | 40 files of 195 diverge today; the classes below |
| `ng` | `serde_yaml_ng` 0.10.0, same shape | item for item identical to `norway` (checked, not assumed) |
| `saphyr` | `saphyr` 0.2.2 as delivered | 33 files; the same classes, plus fixture 24 — the BOM survives into the first key |
| `saphyr-go` | that parser plus the semantic layer in `crates/flowcraft-utils/src/decode.rs` | **adopted** — byte-identical to core over the whole corpus, refusals' wording included |

Those counts are measured with the allowlist off, because `known-divergences.txt`
records where the *adopted* port may disagree with core and a candidate is not
that port: a candidate run compares raw and reports every disagreement, so
`CONFORMANCE_BACKEND=norway make conformance` exits 1 — for a measurement that
is the expected answer rather than a failure. Only `saphyr-go` gates.

What the two serde forks get wrong, by the fixture that shows it:

- **YAML 1.1 booleans.** `yes`, `on`, `y`, `Y`, `N` are booleans to core
  (fixture 02) and strings to them — as values *and* as keys, so a mapping
  whose key is `y` has the key `"true"` on the Go side (fixtures 10, 21, 38).
  This is not a corner case: it is every `on:` in `.github/workflows/*.yml`.
- **YAML 1.1 numbers.** `010` stays the string `"010"` instead of being the
  octal 8, `08` is not a number at all, `1_000` keeps its underscore
  (fixture 04).
- **Integers past int64.** Core rounds through float64 and converts; they
  refuse the document (fixture 07).
- **Tags.** An unknown tag is an error and `!!binary` is not applicable, where
  core tags the scalar and converts it (fixture 28).
- **Duplicate keys.** They keep the last one silently; core refuses the
  document (fixture 12).
- **Collapsing keys.** Two keys that spell one JSON key — `true` and `yes` in
  fixture 09, `1` and `"1"` in 44–50 — are one key too many for core and no
  problem for them.
- **Merge keys.** They keep `<<` as a plain entry; core refuses the document
  (fixture 11).
- **Special floats.** `.inf`, `-.inf` and `.nan` become `null`; core refuses
  the document (fixture 06).
- **YAML directives.** `%YAML 1.2` is refused by core and accepted by them
  (fixture 40).

Bare `saphyr` is not simply those forks with fewer bugs: it refuses documents
they convert (fixtures 06, 09) and converts documents core refuses (27, 44–50),
and on the BOM it differs from both. The parser is what the adopted backend
took from it; the semantics — the part that has to match core — were written
out by hand on top.

## What this gate refuses to do

**It will not compare against an oracle that is not reproducible.** A gate
anchored on a coin flip gates nothing, so the oracle is checked several ways.

*Enumerated evidence.* Where a mapping has two distinct keys that collapse onto
one JSON key, the conversion result depends on Go's map iteration order, so the
document has no defined conversion at all. The oracle says which keys collide
itself, via `key_collisions`, and refuses such a document; the gate takes that
single-run fact and demands the same answer from the port: every file the
oracle flags must come back refused with the collapse refusal (`convert to the
same JSON key`), and the port must refuse it too or the diff is
`error-vs-value`. No sampling and no luck. Today that is fixtures 44-50; the
class is wider than int-vs-string, because the key spelling is lossy in several
ways — see "Collapsing mapping keys" below.

*Sampling.* Nondeterminism this gate cannot enumerate is caught by running the
oracle over the whole corpus 25 times and requiring every file to come out the
same. The defaults are set from measurement, not taste: the minority outcome of
a collapsing file appeared in 47 of 400 runs (≈1/8, Go's map iteration
start-offset distribution for a small map), so the five runs this started with
missed it in 9 of 20 trials. Twenty-five runs cut that to roughly one invocation
in twenty-five — and that class is no longer sampled for at all, now that it is
enumerated as evidence and refused; the runs back the nondeterminism this gate
cannot name. `CONFORMANCE_ORACLE_RUNS` overrides it.

**It will not accept an exemption that stopped being true.** Files listed as
`oracle-unstable` are re-run on their own 100 times and must still disagree. A
stale excuse fails the gate exactly like a mismatch does, and a file with
evidence must not be listed at all: its refusal, not a run count, is what
describes it.

**It will not let a record go missing quietly.** A probe that dies halfway
through the corpus reports `missing-record` per file rather than passing on the
files it did emit. The probe is also run twice and byte-compared: the Go side
can only be sampled, but a Rust map is seeded per process, so an unordered
collection leaking into the output would show up immediately.

**The checkers are tested too.** `tools/selftest.py` drives each failure mode
against a deliberately broken input — an evidenced collapse that is not
refused, an evidenced file listed as `oracle-unstable`, a stale exemption,
unlisted nondeterminism, an `error-vs-value` mismatch, a missing record, a
stale allowlist entry, a refusal worded differently and a stale `error-text`
entry — with the accepted case beside it. Run it directly:

```sh
python3 rust/conformance/tools/selftest.py
```

## Collapsing mapping keys

`utils.ToJSON` refuses a YAML document in which two distinct keys of one
mapping are spelled the same JSON key. The spelling is lossy in more than one
way, so the class is wider than int-vs-string:

- an int and a string: `1` and `"1"` both become `"1"` (fixture 44);
- a bool and a string: `true` and `"true"` (fixture 45);
- an int and a float: `1` and `1.0` (fixture 46);
- two floats at float32 precision: `1` and `1.0000000001` (fixture 47);
- a YAML 1.1 octal and its decimal string: `010` is the int 8, so it collides
  with `"8"` (fixture 48);
- an infinity and its string: `.inf` and `".inf"` (fixture 49);
- the same collapse one mapping deeper (fixture 50).

fixture 51 is the control: a non-string key with nothing to collide with must
keep converting normally. The refusal is by key, not by value: even when the
two values are equal the document still holds one JSON key twice, which would
be for whichever JSON library reads it next to resolve rather than for the
document to state.

Why this cannot be tolerated: go-yaml spells mapping keys while iterating a Go
map, so which of the two values survives is decided by map iteration order —
per process. Measured over 400 runs of the `1`-against-`"1"` case, one value
survived 353 times and the other 47, the start-offset distribution of a small
Go map. The old port resolved the collapse deterministically (last key wins,
document order) and diff.py accepted that as `oracle-unstable-hit`; core now
refuses it and the port refuses it the same way, so the two sides are
`both-error`.

## Categories

| Category | Meaning |
| --- | --- |
| `exact` | byte-identical to the Go output |
| `value-equal` | same value; number or escaping form differs (checked separately — form changes break typed decoding downstream) |
| `both-error` | both sides refused; `errortext.py` compares the words |
| `error-vs-value` | one side refused, the other returned a value |
| `VALUE-DIFF` | both returned values, and they differ |
| `unparseable` | a side returned something that is not JSON |
| `missing-record` | no record for that (file, backend) |
| `oracle-unstable-hit` / `-miss` | the oracle disagrees with itself; the port landed on one of its results / somewhere else |

## Refusals, and which of them are wording

`diff.py` treats a refusal as one observation — "both sides refused" — and
`errortext.py` then compares the words, because a refusal *is* what a caller
reads. What the port reproduces:

- core's own sentences: the `config utils: ` wrapper, the unusable-key
  sentence (every unspellable key of the document, named once each, sorted —
  `~` and an integer past int64, fixtures 52, 53), and the collapsing-key
  sentence (every key of the group, fixtures 44–50, 58–60);
- go-yaml's refusals where the document alone decides them: a repeated key
  with its line and its `%#v` (fixtures 12, 54–57), a value JSON cannot write
  (`json: unsupported value: …`, fixtures 06, 61), a scalar a tag cannot
  decode and a merge whose value is not a mapping (fixtures 67, 68), a
  sequence used as a key (fixtures 39, 66);
- libyaml's reader, which both sides run before the scanner: a control
  character anywhere in a document, and the four ways a character can fail to
  be UTF-8 (fixtures 72–75).

The reader's *encoding* is the one capability it does not have: libyaml picks
UTF-16 from a byte-order mark and decodes it, so core converts such a document
where saphyr's parser — which reads UTF-8 only — makes the port refuse it
(fixtures 77, 78).

What it does not, and why each one is listed in `known-divergences.txt` as
`error-text`:

- the *scanner's* wording. saphyr and libyaml find the same faults and say so
  in their own words and their own positions (fixtures 18, 69);
- `%#v` of a mapping used as a key. `fmt` orders a map's keys by the addresses
  of their type descriptors once the concrete types differ, so the sentence
  is not a function of the document (fixture 65; all-string keys *are*
  ordered by value and are reproduced — fixture 66).

The remaining divergences are values rather than wording. A mapping with two
`<<` keys keeps one merge (fixture 71, `VALUE-DIFF`): the conversion walks the
*loaded tree*, which is a map, so a document a map cannot represent loses
something there. An alias used as an implicit key (fixture 70) and a UTF-16
document (fixtures 77, 78) are the two the parser cannot read at all. The
first of those names its own fix: a conversion driven by the parser's events,
which is a step of its own.

Today's numbers: 150 exact, 41 `both-error` (38 of them word for word, 3
exempted), 4 allowlisted divergences, and no `VALUE-DIFF`,
`error-vs-value`, `value-equal`, `unparseable`, `missing-record` or
`oracle-unstable-hit` outside the allowlist.

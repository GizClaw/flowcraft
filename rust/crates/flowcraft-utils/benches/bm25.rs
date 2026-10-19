//! Baselines for [`bm25`](flowcraft_utils::bm25): what building an index costs,
//! and what a query costs, at the size its own docs name -- a few hundred to a
//! few thousand short documents.
//!
//! These are baselines, not gates. Nothing here asserts a number, and nothing in
//! `cargo test` runs them; `cargo bench -p flowcraft-utils` is the whole
//! audience. What they exist for is the one question a port has to keep asking:
//! core's index was cheap enough to rebuild on a catalog change and small enough
//! to keep in memory, and a Rust port that traded either away would be a
//! regression the contract dump cannot see -- the dump compares scores and order,
//! which are the same at any speed.
//!
//! The four groups answer that question in its four parts:
//!
//! - `tokenize` is the splitter alone, on the two shapes it has code for: an
//!   ASCII description, and a CJK one where every character is also a bigram;
//! - `build` is `Index::new` over the whole corpus, which is the cost of
//!   cataloguing a tool set from raw text;
//! - `build/from_terms` is the same corpus through `Index::from_terms`, the
//!   path a caller takes when the tokenized fields were persisted. The gap
//!   between it and `build` is what tokenizing costs, measured rather than
//!   inferred;
//! - `search` is the query side, in the shapes the callers use: one term, three
//!   terms, a prefix fragment -- the expensive one, because it scans the term
//!   dictionary rather than looking a term up -- and a limited query, which is
//!   the shape the tool catalog actually calls.
//!
//! The corpus is generated, not read from disk: a benchmark whose output depends
//! on a file someone might edit measures the file. It is a deterministic LCG over
//! a fixed vocabulary, so two runs on two machines compare.

use std::collections::BTreeMap;

use criterion::{BenchmarkId, Criterion, Throughput, criterion_group, criterion_main};
use flowcraft_utils::bm25::{Doc, Index, Options, TermDoc, tokenize};

/// The word pool the generated descriptions draw from.
///
/// Deliberately ordinary: overlapping, repeated, and with terms that prefix each
/// other (`read`/`reader`, `list`/`listen`), because a query's prefix scan does
/// nothing at all on a vocabulary with no prefixes in it.
const VOCABULARY: &[&str] = &[
    "read",
    "reader",
    "write",
    "writer",
    "list",
    "listen",
    "search",
    "seek",
    "fetch",
    "fill",
    "open",
    "close",
    "copy",
    "move",
    "delete",
    "create",
    "update",
    "patch",
    "run",
    "stop",
    "start",
    "status",
    "file",
    "files",
    "folder",
    "directory",
    "path",
    "text",
    "line",
    "word",
    "image",
    "video",
    "audio",
    "stream",
    "buffer",
    "socket",
    "network",
    "request",
    "response",
    "header",
    "body",
    "query",
    "result",
    "index",
    "cache",
    "store",
    "load",
    "save",
    "parse",
    "format",
    "encode",
    "decode",
    "json",
    "yaml",
    "markdown",
    "html",
    "css",
    "script",
    "shell",
    "command",
    "process",
    "thread",
    "session",
    "token",
    "secret",
    "policy",
    "rule",
    "match",
    "filter",
    "sort",
    "group",
    "count",
    "limit",
    "offset",
    "page",
    "cursor",
    "checkpoint",
    "resume",
    "cancel",
    "retry",
    "timeout",
    "deadline",
    "budget",
    "memory",
    "disk",
    "cpu",
];

/// One CJK description, as a caller writing Chinese would.
const CJK: &str = "读取文件内容并返回文本行 支持目录遍历与路径匹配 缓存查询结果以加速重复搜索 网络请求的重试与超时策略";

/// The corpus sizes: the low end of the regime, and the high end ("a few
/// thousand").
const SIZES: [usize; 2] = [300, 3_000];

/// How many words a generated description holds, at least and at most.
const SHORTEST: usize = 6;
const LONGEST: usize = 40;

/// One group per question, in the order the docs list them.
fn benches(criterion: &mut Criterion) {
    tokenizing(criterion);
    building(criterion);
    searching(criterion);
}

fn tokenizing(criterion: &mut Criterion) {
    let ascii = vocabulary_text(LONGEST * 4);
    let mut group = criterion.benchmark_group("tokenize");
    group.throughput(Throughput::Bytes(ascii.len() as u64));
    group.bench_function("ascii", |bench| bench.iter(|| tokenize(&ascii)));
    group.throughput(Throughput::Bytes(CJK.len() as u64));
    group.bench_function("cjk", |bench| bench.iter(|| tokenize(CJK)));
    group.finish();
}

fn building(criterion: &mut Criterion) {
    let mut group = criterion.benchmark_group("build");
    for size in SIZES {
        let docs = corpus(size);
        group.throughput(Throughput::Elements(size as u64));
        group.bench_with_input(BenchmarkId::from_parameter(size), &docs, |bench, docs| {
            bench.iter(|| indexed(docs));
        });
    }
    group.finish();

    let mut group = criterion.benchmark_group("build/from_terms");
    for size in SIZES {
        let terms = terms(&corpus(size));
        group.throughput(Throughput::Elements(size as u64));
        group.bench_with_input(BenchmarkId::from_parameter(size), &terms, |bench, terms| {
            bench.iter(|| indexed_terms(terms));
        });
    }
    group.finish();
}

fn searching(criterion: &mut Criterion) {
    // One index at the high end: what a search costs is what the postings of the
    // terms in it cost, and a smaller corpus would measure a shorter list rather
    // than a different implementation.
    let docs = corpus(SIZES[1]);
    let index = indexed(&docs);

    let mut group = criterion.benchmark_group("search");
    group.throughput(Throughput::Elements(docs.len() as u64));
    group.bench_function("one-term", |bench| {
        bench.iter(|| index.search("directory", None));
    });
    group.bench_function("three-terms", |bench| {
        bench.iter(|| index.search("file path search", None));
    });
    // `read` is a word in the vocabulary and a prefix of `reader`: both the
    // exact lookup and the scan come back with hits, which is the case the
    // prefix weight exists for.
    group.bench_function("prefix", |bench| {
        bench.iter(|| index.search("read", None));
    });
    group.bench_function("limited", |bench| {
        bench.iter(|| index.search("file path search", Some(10)));
    });
    group.finish();
}

/// Build an index from raw documents, or say what the corpus broke on.
///
/// A `panic!` rather than `expect`: the workspace lints refuse the panicking
/// accessors, and a corpus that does not index has no measurement to report.
fn indexed(docs: &[Doc]) -> Index {
    match Index::new(docs, Options::default()) {
        Ok(index) => index,
        Err(err) => panic!("the bench corpus must index: {err}"),
    }
}

/// The same, from a corpus a caller had already tokenized.
fn indexed_terms(terms: &[TermDoc]) -> Index {
    match Index::from_terms(terms, Options::default()) {
        Ok(index) => index,
        Err(err) => panic!("the bench corpus must index: {err}"),
    }
}

/// A corpus of `size` short documents, the same on every run and every machine.
fn corpus(size: usize) -> Vec<Doc> {
    let mut generator = Lcg::new(0x5eed);
    (0..size)
        .map(|number| {
            let words = SHORTEST + generator.below(LONGEST - SHORTEST);
            Doc::new(
                format!("doc_{number}"),
                format!("tool_{number}"),
                vocabulary_text(words),
            )
        })
        .collect()
}

/// The tokenized form of a corpus, as a caller who persisted it would have.
fn terms(docs: &[Doc]) -> Vec<TermDoc> {
    docs.iter()
        .map(|doc| TermDoc {
            id: doc.id.clone(),
            name: frequencies(&doc.name),
            text: frequencies(&doc.text),
        })
        .collect()
}

/// One field's term frequencies.
fn frequencies(text: &str) -> BTreeMap<String, i32> {
    let mut counts: BTreeMap<String, i32> = BTreeMap::new();
    for term in tokenize(text) {
        *counts.entry(term).or_default() += 1;
    }
    counts
}

/// `words` words of the vocabulary, space separated.
fn vocabulary_text(words: usize) -> String {
    let mut generator = Lcg::new(words as u64 + 1);
    let mut text = String::with_capacity(words * 8);
    for index in 0..words {
        if index > 0 {
            text.push(' ');
        }
        text.push_str(VOCABULARY[generator.below(VOCABULARY.len())]);
    }
    text
}

/// A linear congruential generator, so the corpus is the same everywhere without
/// a dependency whose own choice of algorithm could change it.
struct Lcg {
    state: u64,
}

impl Lcg {
    fn new(seed: u64) -> Lcg {
        Lcg { state: seed }
    }

    /// A number below `bound`, which must not be zero.
    fn below(&mut self, bound: usize) -> usize {
        // Numerical Recipes' constants: a full period, and no multiply that
        // overflows out of `u64`.
        self.state = self
            .state
            .wrapping_mul(6_364_136_223_846_793_005)
            .wrapping_add(1_442_695_040_888_963_407);
        (self.state >> 33) as usize % bound
    }
}

criterion_group!(bm25, benches);
criterion_main!(bm25);

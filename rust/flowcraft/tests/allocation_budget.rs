#![allow(clippy::unwrap_used, clippy::expect_used)]

//! The allocation budget: the one property a compiler cannot check.
//!
//! The P2 class of defects is "the same value copied once more than necessary".
//! None of them changes an observable result, so no behaviour test can catch a
//! regression — the only honest gate counts allocations. Three properties are
//! pinned here, and they are the three the caches in `caps::tool` exist for:
//!
//! 1. a catalog whose contents are unchanged serves **the same** snapshot (an
//!    `Arc` clone, zero allocations), which is what `DynamicSession::rank` and
//!    the per-round visibility computation key their reuse on;
//! 2. a budgeted session copies only the definitions it actually shows — a
//!    1000-tool catalog narrowed to 30 clones 30, not 1000;
//! 3. a mutation publishes a new snapshot, so (1) can never serve stale tools.
//!
//! One `#[test]` on purpose: the counter is process-wide, so a second test in
//! this binary would run concurrently with this one and pollute the readings.
//! Nothing measured here awaits, so the runtime cannot allocate mid-region
//! either.
//!
//! The workspace forbids `unsafe_code`, so the counting allocator comes from
//! `stats_alloc` (a dev-dependency) rather than from this crate: only the
//! `#[global_allocator]` declaration is local, and it contains no `unsafe`.

use std::sync::Arc;

use stats_alloc::{INSTRUMENTED_SYSTEM, Region, StatsAlloc};

use flowcraft::caps::tool::{
    Budget, CallContext, Catalog, ConflictPolicy, DynamicSession, Exposure, FuncTool, Policy,
    Registry, SearchHit, Session, StaticSource, Tool,
};
use flowcraft::message::text_content;
use flowcraft::proto::message as msg;

#[global_allocator]
static GLOBAL: &StatsAlloc<std::alloc::System> = &INSTRUMENTED_SYSTEM;

/// A catalog large enough that a per-call copy is unmistakable in the readings.
const TOOLS: usize = 1000;
/// Per-tool description size. `TOOLS * DESCRIPTION` is the "old behaviour" floor:
/// copying every definition per call cannot come in under it.
const DESCRIPTION: usize = 200;
/// What the session below is allowed to show in one round.
const VISIBLE: usize = 30;

fn definition(index: usize) -> msg::ToolDefinition {
    msg::ToolDefinition {
        name: format!("tool-{index:04}"),
        description: "d".repeat(DESCRIPTION),
        input_schema: br#"{"type":"object"}"#.to_vec(),
    }
}

fn tool(index: usize) -> Arc<dyn Tool> {
    Arc::new(FuncTool::new(
        definition(index),
        Arc::new(|_context: &CallContext, _arguments: &str| Ok(text_content("ok"))),
    ))
}

async fn catalog() -> Arc<Registry> {
    let mut source = StaticSource::new();
    for index in 0..TOOLS {
        source.push(tool(index));
    }
    Registry::from_sources(&[Arc::new(source)], ConflictPolicy::Error)
        .await
        .expect("registry")
}

#[tokio::test]
async fn the_caches_hold_their_budgets() {
    let registry = catalog().await;

    // (1) A stable catalog is one allocation, once.
    let first = registry.definitions();
    assert_eq!(first.len(), TOOLS);
    let region = Region::new(GLOBAL);
    let second = registry.definitions();
    let change = region.change();
    assert!(
        Arc::ptr_eq(&first, &second),
        "an unchanged catalog must serve the same snapshot"
    );
    assert_eq!(
        change.allocations, 0,
        "reusing a snapshot must not allocate: {:?}",
        change
    );
    assert_eq!(
        change.bytes_allocated, 0,
        "reusing a snapshot must not copy: {:?}",
        change
    );

    // (2) A budgeted session copies only what it shows. Copying the whole catalog
    // and then filtering would allocate at least `TOOLS * DESCRIPTION` bytes.
    let policy = Policy {
        default: Exposure::Always,
        budget: Budget {
            max_definitions: VISIBLE,
            max_bytes: 1 << 20,
        },
        ..Policy::default()
    };
    let session = DynamicSession::new(registry.clone(), policy).expect("session");
    // Warm whatever the first call allocates lazily (nothing measured).
    assert_eq!(session.definitions().len(), VISIBLE);
    let region = Region::new(GLOBAL);
    let visible = session.definitions();
    let change = region.change();
    assert_eq!(visible.len(), VISIBLE);
    assert!(
        change.bytes_allocated < TOOLS * DESCRIPTION,
        "a {VISIBLE}-tool round copied more than a whole-catalog round would: {:?}",
        change
    );

    // (3) A mutation publishes a new snapshot: (1) must never serve stale tools.
    registry.add(tool(TOOLS + 1)).expect("add");
    let after = registry.definitions();
    assert!(
        !Arc::ptr_eq(&second, &after),
        "a mutation must publish a new snapshot"
    );
    assert_eq!(after.len(), TOOLS + 1);
    assert!(
        after
            .iter()
            .any(|definition| definition.name == format!("tool-{:04}", TOOLS + 1))
    );

    // (4) A repeated query reuses the search index. Building it tokenizes every
    // searchable tool, so a second search that rebuilt it would pay the whole
    // corpus price again — that is the P2-2 defect.
    let hit = |hits: &[SearchHit], name: &str| hits.iter().any(|hit| hit.name == name);
    let searchable = Policy {
        // Only `direct` and `deferred` are searchable.
        default: Exposure::Direct,
        ..Policy::default()
    };
    let session = DynamicSession::new(registry.clone(), searchable.clone()).expect("session");
    // Warm: this call builds the index (nothing measured).
    assert!(hit(
        &session.search("tool-0999", 32).await.expect("search"),
        "tool-0999"
    ));
    let region = Region::new(GLOBAL);
    let again = session.search("tool-0999", 32).await.expect("search");
    let warm = region.change();
    assert!(hit(&again, "tool-0999"));

    // The control: a session that has never searched must index the corpus
    // first, and that build is what a per-query index would pay every time.
    let cold = DynamicSession::new(registry.clone(), searchable).expect("session");
    let region = Region::new(GLOBAL);
    let cold_hits = cold.search("tool-0999", 32).await.expect("search");
    let cold_change = region.change();
    assert!(hit(&cold_hits, "tool-0999"));
    assert!(
        warm.allocations * 4 < cold_change.allocations,
        "reusing the index saved little: warm {:?} vs cold {:?}",
        warm,
        cold_change
    );

    // (5) And a mutation invalidates it, or a tool registered after a search
    // would never be findable.
    assert!(
        !hit(&session.search("zebra", 8).await.expect("search"), "zebra"),
        "zebra is not registered yet"
    );
    registry
        .add(Arc::new(FuncTool::new(
            msg::ToolDefinition {
                name: "zebra".to_owned(),
                description: "stripes, black and white".to_owned(),
                input_schema: br#"{"type":"object"}"#.to_vec(),
            },
            Arc::new(|_context: &CallContext, _arguments: &str| Ok(text_content("ok"))),
        )))
        .expect("add");
    assert!(
        hit(&session.search("zebra", 8).await.expect("search"), "zebra"),
        "a catalog change must rebuild the index"
    );
}

#![allow(clippy::unwrap_used, clippy::expect_used)]

//! End-to-end tests for the composition kernel: planning diagnostics,
//! construction order, lifecycle order, and failure teardown.

use std::sync::{Arc, Mutex};

use flowcraft::error::ErrorKind;
use flowcraft::resource::{
    BoxFuture, BuildCtx, Builder, ExternalResource, Factory, Phase, Registry, Resource, Spec,
    optional, require, require_many,
};
use flowcraft::resource::{Deployment, Kind};
use serde_json::json;

/// A value whose lifecycle appends to a shared log.
struct Traced {
    name: String,
    log: Arc<Mutex<Vec<String>>>,
}

impl Traced {
    fn new(name: impl Into<String>, log: &Arc<Mutex<Vec<String>>>) -> Self {
        Self {
            name: name.into(),
            log: log.clone(),
        }
    }
}

impl Resource for Traced {
    fn start(self: Arc<Self>) -> BoxFuture<'static, Result<(), flowcraft::error::Error>> {
        let name = self.name.clone();
        let log = self.log.clone();
        Box::pin(async move {
            log.lock().expect("log").push(format!("start:{name}"));
            Ok(())
        })
    }

    fn shutdown(self: Arc<Self>) -> BoxFuture<'static, Result<(), flowcraft::error::Error>> {
        let name = self.name.clone();
        let log = self.log.clone();
        Box::pin(async move {
            log.lock().expect("log").push(format!("shutdown:{name}"));
            Ok(())
        })
    }
}

/// A second output type, used to provoke a type mismatch.
struct Other;

impl Resource for Other {}

/// How a helper factory materialises its concrete output.
trait Make: Resource {
    fn make(kind: &Kind, log: &Arc<Mutex<Vec<String>>>) -> Self;
}

impl Make for Traced {
    fn make(kind: &Kind, log: &Arc<Mutex<Vec<String>>>) -> Self {
        Traced::new(kind.to_string(), log)
    }
}

impl Make for Other {
    fn make(_kind: &Kind, _log: &Arc<Mutex<Vec<String>>>) -> Self {
        Other
    }
}

struct FactoryOf<T> {
    spec: Spec,
    log: Arc<Mutex<Vec<String>>>,
    fail_build: bool,
    marker: std::marker::PhantomData<T>,
}

impl<T: Resource + Make> Factory for FactoryOf<T> {
    type Output = T;

    fn spec(&self) -> Spec {
        self.spec.clone()
    }

    fn build<'a>(
        &'a self,
        _ctx: BuildCtx<'a>,
    ) -> BoxFuture<'a, Result<Arc<T>, flowcraft::error::Error>> {
        Box::pin(async move {
            if self.fail_build {
                return Err(flowcraft::error::Error::internal("build failed"));
            }
            let value = T::make(&self.spec.kind, &self.log);
            Ok(Arc::new(value))
        })
    }
}

fn spec(kind: &str) -> Spec {
    Spec::new(Kind::new(kind).expect("kind"))
}

fn traced_factory<T: Resource + Make>(spec: Spec, log: &Arc<Mutex<Vec<String>>>) -> FactoryOf<T> {
    FactoryOf {
        spec,
        log: log.clone(),
        fail_build: false,
        marker: std::marker::PhantomData,
    }
}

fn document(resources: &[(&str, serde_json::Value)]) -> Deployment {
    let body = json!({
        "version": "v1",
        "resources": resources
            .iter()
            .map(|(name, value)| ((*name).to_owned(), value.clone()))
            .collect::<serde_json::Map<_, _>>(),
    });
    flowcraft::resource::decode::deployment(&serde_json::to_vec(&body).expect("json")).expect("doc")
}

fn document_with(extra: serde_json::Value) -> Deployment {
    flowcraft::resource::decode::deployment(&serde_json::to_vec(&extra).expect("json"))
        .expect("doc")
}

#[tokio::test]
async fn plan_orders_dependencies_and_builds_them() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    registry
        .register(traced_factory::<Traced>(spec("test.Source"), &log))
        .expect("register");
    let mut consumer = spec("test.Consumer");
    consumer.deps = vec![require::<Traced>("source")];
    registry
        .register(traced_factory::<Traced>(consumer, &log))
        .expect("register");

    let deployment = document(&[
        (
            "dst",
            json!({"kind": "test.Consumer", "deps": {"source": "src"}}),
        ),
        ("src", json!({"kind": "test.Source"})),
    ]);

    let built = Builder::new(Arc::new(registry))
        .deploy(deployment)
        .await
        .expect("deploy");
    assert_eq!(built.names(), &["src".to_owned(), "dst".to_owned()]);
    built.start().await.expect("start");
    built.shutdown().await.expect("shutdown");
    assert_eq!(
        *log.lock().expect("log"),
        vec![
            "start:test.Source",
            "start:test.Consumer",
            "shutdown:test.Consumer",
            "shutdown:test.Source",
        ]
    );
}

#[tokio::test]
async fn cycle_is_reported_without_building() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    let mut a = spec("test.A");
    a.deps = vec![require::<Traced>("other")];
    let mut b = spec("test.B");
    b.deps = vec![require::<Traced>("other")];
    registry
        .register(traced_factory::<Traced>(a, &log))
        .expect("register");
    registry
        .register(traced_factory::<Traced>(b, &log))
        .expect("register");

    let deployment = document(&[
        ("a", json!({"kind": "test.A", "deps": {"other": "b"}})),
        ("b", json!({"kind": "test.B", "deps": {"other": "a"}})),
    ]);
    let errors = Builder::new(Arc::new(registry))
        .plan(deployment)
        .expect_err("cycle");
    assert!(
        errors.iter().any(|error| error.message.contains("cycle")),
        "{errors:?}"
    );
    assert!(log.lock().expect("log").is_empty());
}

#[tokio::test]
async fn missing_and_undeclared_dependencies_are_both_reported() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    registry
        .register(traced_factory::<Traced>(spec("test.Source"), &log))
        .expect("register");
    let mut consumer = spec("test.Consumer");
    consumer.deps = vec![require::<Traced>("source"), require::<Traced>("extra")];
    registry
        .register(traced_factory::<Traced>(consumer, &log))
        .expect("register");

    let deployment = document(&[
        ("src", json!({"kind": "test.Source"})),
        (
            "dst",
            json!({"kind": "test.Consumer", "deps": {"bogus": "src"}}),
        ),
    ]);
    let errors = Builder::new(Arc::new(registry))
        .plan(deployment)
        .expect_err("plan");
    let messages: Vec<&str> = errors.iter().map(|error| error.message.as_str()).collect();
    assert!(
        messages
            .iter()
            .any(|message| message.contains("not declared")),
        "{messages:?}"
    );
    assert!(
        messages
            .iter()
            .any(|message| message.contains("missing required dependency")),
        "{messages:?}"
    );
}

#[tokio::test]
async fn type_mismatch_is_rejected_at_plan_time() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    registry
        .register(traced_factory::<Other>(spec("test.Other"), &log))
        .expect("register");
    let mut consumer = spec("test.Consumer");
    consumer.deps = vec![require::<Traced>("source")];
    registry
        .register(traced_factory::<Traced>(consumer, &log))
        .expect("register");

    let deployment = document(&[
        ("other", json!({"kind": "test.Other"})),
        (
            "dst",
            json!({"kind": "test.Consumer", "deps": {"source": "other"}}),
        ),
    ]);
    let errors = Builder::new(Arc::new(registry))
        .plan(deployment)
        .expect_err("type mismatch");
    assert!(
        errors
            .iter()
            .any(|error| error.message.contains("provides")),
        "{errors:?}"
    );
}

#[tokio::test]
async fn failure_shuts_down_what_was_already_built() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    registry
        .register(traced_factory::<Traced>(spec("test.Source"), &log))
        .expect("register");
    let mut broken = spec("test.Broken");
    // Depend on the source so it is built first, then fail: the kernel must
    // tear the source down again.
    broken.deps = vec![require::<Traced>("src")];
    registry
        .register(FactoryOf::<Traced> {
            spec: broken,
            log: log.clone(),
            fail_build: true,
            marker: std::marker::PhantomData,
        })
        .expect("register");
    let deployment = document(&[
        ("src", json!({"kind": "test.Source"})),
        (
            "broken",
            json!({"kind": "test.Broken", "deps": {"src": "src"}}),
        ),
    ]);
    let error = Builder::new(Arc::new(registry))
        .deploy(deployment)
        .await
        .expect_err("build failure");
    assert_eq!(error.kind(), ErrorKind::Internal);
    assert_eq!(*log.lock().expect("log"), vec!["shutdown:test.Source"]);
}

#[tokio::test]
async fn many_dependency_collects_prefixed_edges() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    registry
        .register(traced_factory::<Traced>(spec("test.Item"), &log))
        .expect("register");
    let mut consumer = spec("test.Sink");
    consumer.deps = vec![require_many::<Traced>("item")];
    registry
        .register(traced_factory::<Traced>(consumer, &log))
        .expect("register");
    let deployment = document(&[
        ("one", json!({"kind": "test.Item"})),
        ("two", json!({"kind": "test.Item"})),
        (
            "sink",
            json!({"kind": "test.Sink", "deps": {"item.a": "one", "item.b": "two"}}),
        ),
    ]);
    let built = Builder::new(Arc::new(registry))
        .deploy(deployment)
        .await
        .expect("deploy");
    assert_eq!(built.names().len(), 3);
}

#[tokio::test]
async fn optional_dependency_may_be_absent() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    let mut consumer = spec("test.Consumer");
    consumer.deps = vec![optional::<Traced>("cache")];
    registry
        .register(traced_factory::<Traced>(consumer, &log))
        .expect("register");
    let built = Builder::new(Arc::new(registry))
        .deploy(document(&[("dst", json!({"kind": "test.Consumer"}))]))
        .await
        .expect("deploy");
    assert_eq!(built.names(), &["dst".to_owned()]);
}

#[tokio::test]
async fn externals_are_injected_and_contract_checked() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    let mut consumer = spec("test.Consumer");
    let mut requirement = require::<Traced>("env");
    requirement.contract = Some("test.Env/v1");
    consumer.deps = vec![requirement];
    registry
        .register(traced_factory::<Traced>(consumer, &log))
        .expect("register");

    let value = Arc::new(Traced::new("external", &log));

    let deployment = document_with(json!({
        "version": "v1",
        "resources": {"dst": {"kind": "test.Consumer", "deps": {"env": "env"}}},
        "externals": [{"name": "env", "contract": "test.Env/v1"}],
    }));

    let built = Builder::new(Arc::new(registry.clone()))
        .with_external(ExternalResource::new("env", "test.Env/v1", value.clone()))
        .deploy(deployment)
        .await
        .expect("deploy");
    assert_eq!(built.names(), &["dst".to_owned()]);

    // Wrong contract: rejected by plan.
    let deployment = document_with(json!({
        "version": "v1",
        "resources": {"dst": {"kind": "test.Consumer", "deps": {"env": "env"}}},
        "externals": [{"name": "env", "contract": "test.Env/v2"}],
    }));
    let errors = Builder::new(Arc::new(registry))
        .with_external(ExternalResource::new("env", "test.Env/v2", value))
        .plan(deployment)
        .expect_err("contract");
    assert!(
        errors
            .iter()
            .any(|error| error.message.contains("contract")),
        "{errors:?}"
    );
}

#[tokio::test]
async fn pre_phase_builds_before_main_and_may_not_consume_it() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    let mut pre = spec("test.Pre");
    pre.phase = Phase::Pre;
    registry
        .register(traced_factory::<Traced>(pre, &log))
        .expect("register");
    let mut main = spec("test.Main");
    main.deps = vec![require::<Traced>("pre")];
    registry
        .register(traced_factory::<Traced>(main, &log))
        .expect("register");

    // The pre value sorts after the main resource by name, yet builds first.
    let built = Builder::new(Arc::new(registry.clone()))
        .deploy(document(&[
            (
                "a-main",
                json!({"kind": "test.Main", "deps": {"pre": "z-pre"}}),
            ),
            ("z-pre", json!({"kind": "test.Pre"})),
        ]))
        .await
        .expect("deploy");
    assert_eq!(built.names(), &["z-pre".to_owned(), "a-main".to_owned()]);

    // The reverse edge is a plan error.
    let mut bad_pre = spec("test.BadPre");
    bad_pre.phase = Phase::Pre;
    bad_pre.deps = vec![require::<Traced>("main")];
    let mut registry = Registry::new();
    registry
        .register(traced_factory::<Traced>(bad_pre, &log))
        .expect("register");
    registry
        .register(traced_factory::<Traced>(spec("test.Main2"), &log))
        .expect("register");
    let errors = Builder::new(Arc::new(registry))
        .plan(document(&[
            ("pre", json!({"kind": "test.BadPre", "deps": {"main": "m"}})),
            ("m", json!({"kind": "test.Main2"})),
        ]))
        .expect_err("pre depends on main");
    assert!(
        errors
            .iter()
            .any(|error| error.message.contains("pre-phase")),
        "{errors:?}"
    );
}

#[tokio::test]
async fn settings_are_expanded_from_the_environment() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    let mut source = spec("test.Source");
    source.validate_settings = Some(|settings| {
        if settings
            .get("root")
            .and_then(|value| value.as_str())
            .is_some()
        {
            Ok(())
        } else {
            Err(flowcraft::error::Error::validation("root is required"))
        }
    });
    registry
        .register(traced_factory::<Traced>(source, &log))
        .expect("register");
    let deployment = document(&[(
        "src",
        json!({"kind": "test.Source", "settings": {"root": "${env:HOME}"}}),
    )]);
    let built = Builder::new(Arc::new(registry))
        .with_resolver(flowcraft::resource::Resolver::new())
        .deploy(deployment)
        .await
        .expect("deploy");
    assert_eq!(built.names(), &["src".to_owned()]);
}

#[tokio::test]
async fn settings_validation_failure_is_a_plan_error() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    let mut source = spec("test.Source");
    source.validate_settings = Some(|settings| {
        if settings.get("root").is_some() {
            Ok(())
        } else {
            Err(flowcraft::error::Error::validation(
                "settings.root is required",
            ))
        }
    });
    registry
        .register(traced_factory::<Traced>(source, &log))
        .expect("register");
    let errors = Builder::new(Arc::new(registry))
        .plan(document(&[("src", json!({"kind": "test.Source"}))]))
        .expect_err("settings");
    assert_eq!(errors[0].path, "resources.src");
    assert!(errors[0].message.contains("settings.root"), "{errors:?}");
}

#[tokio::test]
async fn missing_factory_is_a_plan_error() {
    let errors = Builder::new(Arc::new(Registry::new()))
        .plan(document(&[("ws", json!({"kind": "workspace.Workspace"}))]))
        .expect_err("no factory");
    assert_eq!(errors[0].path, "resources.ws");
    assert!(errors[0].message.contains("no factory"), "{errors:?}");
}

#[tokio::test]
async fn entries_select_what_is_returned() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let mut registry = Registry::new();
    registry
        .register(traced_factory::<Traced>(spec("test.Item"), &log))
        .expect("register");
    let deployment = document_with(json!({
        "version": "v1",
        "resources": {
            "one": {"kind": "test.Item"},
            "two": {"kind": "test.Item"},
        },
        "entries": ["two"],
    }));
    let built = Builder::new(Arc::new(registry))
        .deploy(deployment)
        .await
        .expect("deploy");
    assert_eq!(built.names().len(), 2);
    let entries = built.entries();
    assert_eq!(entries.len(), 1);
    assert_eq!(entries[0].0, "two");
}

//! Planning: the pure, allocation-only phase.
//!
//! `Plan` resolves every `kind/impl` to a factory, checks every declared
//! dependency against what the document supplies, checks every edge's type,
//! rejects cycles, and orders construction. It performs no I/O and constructs
//! nothing, so a configuration error never leaves a half-built graph behind.
//!
//! Every problem is reported at once. A caller that wants a preflight (the
//! `flowcraftd check` command) reads exactly this list.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use crate::error::Error;
use crate::resource::Deployment;
use serde_json::Value;

use crate::resource::build_type_of;
use crate::resource::lifecycle::ExternalResource;
use crate::resource::registry::Registry;
use crate::resource::spec::{ErasedFactory, Phase, Spec};

/// One planning problem, addressable by document path.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct PlanError {
    /// `resources.tools.deps.tool`, or empty for a document-level problem.
    pub path: String,
    /// What is wrong.
    pub message: String,
}

impl PlanError {
    fn new(path: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            path: path.into(),
            message: message.into(),
        }
    }
}

impl std::fmt::Display for PlanError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if self.path.is_empty() {
            f.write_str(&self.message)
        } else {
            write!(f, "{}: {}", self.path, self.message)
        }
    }
}

/// One resolved edge.
pub(crate) struct PlanEdge {
    /// The requirement that claims this edge.
    pub requirement: &'static str,
    /// The edge name as written in the document.
    pub name: String,
    /// The producer.
    pub producer: String,
    /// Whether the producer is an external (leaf) or a resource.
    pub external: bool,
    /// Whether this edge contributes to a many-dependency.
    pub many: bool,
}

/// One planned resource.
pub(crate) struct PlanNode {
    pub factory: Arc<dyn ErasedFactory>,
    pub spec: Spec,
    pub edges: Vec<PlanEdge>,
    pub settings: Option<Value>,
}

/// A checked, immutable deployment.
pub struct Plan {
    pub(crate) deployment: Deployment,
    pub(crate) nodes: BTreeMap<String, PlanNode>,
    pub(crate) order: Vec<String>,
    pub(crate) externals: BTreeMap<String, ExternalResource>,
}

impl std::fmt::Debug for Plan {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Plan")
            .field("order", &self.order)
            .field(
                "resources",
                &self.deployment.resources.keys().collect::<Vec<_>>(),
            )
            .field("externals", &self.externals.keys().collect::<Vec<_>>())
            .finish_non_exhaustive()
    }
}

impl Plan {
    /// Construction order: pre-phase first, then main, dependencies first.
    #[must_use]
    pub fn order(&self) -> &[String] {
        &self.order
    }

    /// The planned document.
    #[must_use]
    pub fn deployment(&self) -> &Deployment {
        &self.deployment
    }

    /// The injected externals.
    #[must_use]
    pub fn externals(&self) -> &BTreeMap<String, ExternalResource> {
        &self.externals
    }

    /// The spec of one planned resource.
    #[must_use]
    pub fn spec(&self, name: &str) -> Option<&Spec> {
        self.nodes.get(name).map(|node| &node.spec)
    }
}

/// Plan a deployment against a registry and a set of injected externals.
pub(crate) fn plan(
    registry: &Registry,
    externals: &[ExternalResource],
    deployment: Deployment,
) -> std::result::Result<Plan, Vec<PlanError>> {
    if let Err(error) = deployment.validate() {
        return Err(vec![PlanError::new("", error.to_string())]);
    }

    let mut errors = Vec::new();

    // Index injected externals, validating each in isolation.
    let mut external_index: BTreeMap<String, ExternalResource> = BTreeMap::new();
    for external in externals {
        if let Err(error) = external.validate() {
            errors.push(PlanError::new(
                format!("externals.{}", external.name),
                error.to_string(),
            ));
            continue;
        }
        if deployment.resources.contains_key(&external.name) {
            errors.push(PlanError::new(
                format!("externals.{}", external.name),
                "injected external shadows a resource",
            ));
            continue;
        }
        external_index.insert(external.name.clone(), external.clone());
    }
    // A declared external that the host did not inject is a missing value, not
    // a document error: report it once, at its declaration.
    for declared in &deployment.externals {
        if !external_index.contains_key(&declared.name) {
            errors.push(PlanError::new(
                format!("externals.{}", declared.name),
                format!(
                    "external {:?} is declared but not injected by the host",
                    declared.name
                ),
            ));
        }
    }

    let mut nodes: BTreeMap<String, PlanNode> = BTreeMap::new();
    for (name, resource) in &deployment.resources {
        let path = format!("resources.{name}");
        let Some(factory) = registry.get(&resource.kind, resource.imp.as_deref()) else {
            errors.push(PlanError::new(
                &path,
                format!("no factory for {}", resource.key()),
            ));
            continue;
        };
        let spec = factory.spec();
        let settings = resource.settings.clone();

        if let Some(validate) = spec.validate_settings {
            let value = settings
                .clone()
                .unwrap_or_else(|| Value::Object(serde_json::Map::new()));
            if let Err(error) = validate(&value) {
                errors.push(PlanError::new(&path, error.to_string()));
            }
        }

        let mut edges = Vec::new();
        let mut claimed: BTreeSet<&'static str> = BTreeSet::new();
        for (edge_name, producer) in &resource.deps {
            let producer_name = producer.as_str();
            let Some(requirement) = spec.deps.iter().find(|item| item.claims(edge_name)) else {
                errors.push(PlanError::new(
                    format!("{path}.deps.{edge_name}"),
                    format!(
                        "dependency {edge_name:?} is not declared by {}",
                        resource.key()
                    ),
                ));
                continue;
            };
            claimed.insert(requirement.name);

            let (external, producer_type, producer_type_name, producer_contract) = if let Some(
                producer_resource,
            ) =
                deployment.resources.get(producer_name)
            {
                match registry.get(&producer_resource.kind, producer_resource.imp.as_deref()) {
                    Some(producer_factory) => (
                        false,
                        producer_factory.build_type(),
                        producer_factory.build_type_name(),
                        producer_factory.spec().contract,
                    ),
                    None => {
                        errors.push(PlanError::new(
                            format!("{path}.deps.{edge_name}"),
                            format!(
                                "depends on {producer_name:?}, which has no factory for {}",
                                producer_resource.key()
                            ),
                        ));
                        continue;
                    }
                }
            } else if let Some(external) = external_index.get(producer_name) {
                (
                    true,
                    build_type_of(&external.value),
                    external.type_name,
                    Some(external.contract.as_str()),
                )
            } else {
                errors.push(PlanError::new(
                        format!("{path}.deps.{edge_name}"),
                        format!(
                            "depends on {producer_name:?}, which is neither a resource nor an injected external"
                        ),
                    ));
                continue;
            };

            if let Some(want) = requirement.ty
                && want != producer_type
            {
                errors.push(PlanError::new(
                    format!("{path}.deps.{edge_name}"),
                    format!(
                        "dependency {edge_name:?} provides {producer_type_name}, but {} requires {}",
                        requirement.name, requirement.ty_name
                    ),
                ));
            }
            if let Some(want) = requirement.contract {
                match producer_contract {
                    Some(have) if have == want => {}
                    Some(have) => errors.push(PlanError::new(
                        format!("{path}.deps.{edge_name}"),
                        format!(
                            "dependency {edge_name:?} has contract {have:?}, but {} requires {want:?}",
                            requirement.name
                        ),
                    )),
                    None => errors.push(PlanError::new(
                        format!("{path}.deps.{edge_name}"),
                        format!(
                            "dependency {edge_name:?} has no contract, but {} requires {want:?}",
                            requirement.name
                        ),
                    )),
                }
            }

            edges.push(PlanEdge {
                requirement: requirement.name,
                name: edge_name.clone(),
                producer: producer_name.to_owned(),
                external,
                many: requirement.many,
            });
        }

        for requirement in &spec.deps {
            if requirement.required && !claimed.contains(requirement.name) {
                errors.push(PlanError::new(
                    format!("{path}.deps.{}", requirement.name),
                    format!("missing required dependency {:?}", requirement.name),
                ));
            }
        }

        nodes.insert(
            name.clone(),
            PlanNode {
                factory,
                spec,
                edges,
                settings,
            },
        );
    }

    // Pre-phase values are built first, so they may not consume a main value.
    for (name, node) in &nodes {
        if node.spec.phase != Phase::Pre {
            continue;
        }
        for edge in &node.edges {
            if let Some(producer) = nodes.get(&edge.producer)
                && producer.spec.phase == Phase::Main
            {
                errors.push(PlanError::new(
                    format!("resources.{name}.deps.{}", edge.name),
                    "a pre-phase resource may not depend on a main-phase resource",
                ));
            }
        }
    }

    let order = match order_resources(&nodes) {
        Ok(order) => order,
        Err(cyclic) => {
            errors.push(PlanError::new(
                "resources",
                format!("dependency cycle among {cyclic:?}"),
            ));
            Vec::new()
        }
    };

    if !errors.is_empty() {
        return Err(errors);
    }

    // Stable partition: pre-phase first, otherwise topological order.
    let mut ordered = Vec::with_capacity(order.len());
    for want_pre in [true, false] {
        for name in &order {
            let is_pre = nodes
                .get(name)
                .is_some_and(|node| node.spec.phase == Phase::Pre);
            if is_pre == want_pre {
                ordered.push(name.clone());
            }
        }
    }

    Ok(Plan {
        deployment,
        nodes,
        order: ordered,
        externals: external_index,
    })
}

/// Kahn's algorithm over resource→resource edges, deterministic by name.
fn order_resources(
    nodes: &BTreeMap<String, PlanNode>,
) -> std::result::Result<Vec<String>, Vec<String>> {
    let mut indegree: BTreeMap<&str, usize> = nodes.keys().map(|name| (name.as_str(), 0)).collect();
    let mut consumers: BTreeMap<&str, BTreeSet<&str>> = BTreeMap::new();
    for (name, node) in nodes {
        for edge in &node.edges {
            if edge.external || !nodes.contains_key(&edge.producer) {
                continue;
            }
            if consumers
                .entry(edge.producer.as_str())
                .or_default()
                .insert(name.as_str())
                && let Some(degree) = indegree.get_mut(name.as_str())
            {
                *degree += 1;
            }
        }
    }

    let mut ready: BTreeSet<&str> = indegree
        .iter()
        .filter(|(_, degree)| **degree == 0)
        .map(|(name, _)| *name)
        .collect();
    let mut order = Vec::with_capacity(nodes.len());
    while let Some(name) = ready.iter().next().copied() {
        ready.remove(name);
        order.push(name.to_owned());
        if let Some(children) = consumers.get(name).cloned() {
            for child in children {
                if let Some(degree) = indegree.get_mut(child) {
                    *degree -= 1;
                    if *degree == 0 {
                        ready.insert(child);
                    }
                }
            }
        }
    }

    if order.len() != nodes.len() {
        let mut remaining: Vec<String> = nodes
            .keys()
            .filter(|name| !order.contains(*name))
            .cloned()
            .collect();
        remaining.sort();
        return Err(remaining);
    }
    Ok(order)
}

/// Renders a plan failure as one error message, for callers that do not keep
/// the list (Build, for example).
pub(crate) fn render_errors(errors: &[PlanError]) -> Error {
    let mut message = String::from("deployment plan failed:");
    for error in errors {
        message.push_str("\n  ");
        message.push_str(&error.to_string());
    }
    Error::validation(message)
}

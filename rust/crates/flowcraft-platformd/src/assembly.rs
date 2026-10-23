//! Assembly: the builtin registry, and the host-owned externals injected before
//! planning.
//!
//! The injection mirrors the Go runtime: an endpoint resource never has to name
//! the listener factory or the grant issuer in its document — the platform
//! supplies them. Keeping it in one place means `check` and `serve` see exactly
//! the same plan.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use flowcraft_cap_sandbox::{
    endpoint as sandbox_endpoint, local as sandbox_local, remote as sandbox_remote,
};
use flowcraft_cap_secret::{
    endpoint as secret_endpoint, env as secret_env, file as secret_file,
    keychain as secret_keychain, memory as secret_memory, remote as secret_remote,
};
use flowcraft_cap_tool::{endpoint as tool_endpoint, remote as tool_remote};
use flowcraft_cap_workspace::{endpoint, local, remote, scoped, sub};
use flowcraft_error::{Error, Result};
use flowcraft_kernel::{Endpoints, Grants, Platform};
use flowcraft_resource::PlanError;
use flowcraft_resource::{Builder, Built, ExternalResource, Registry, Resolver};
use flowcraft_resource::{Deployment, Ref};

use crate::secrets::{DeploymentSecrets, InjectedSecrets};

/// Which capability each endpoint kind serves when settings do not say.
const ENDPOINT_CAPABILITIES: &[(&str, &str)] = &[
    (
        "workspace.Endpoint",
        flowcraft_cap_workspace::endpoint::CAPABILITY,
    ),
    (
        "secret.Endpoint",
        flowcraft_cap_secret::endpoint::CAPABILITY,
    ),
    (
        "sandbox.Endpoint",
        flowcraft_cap_sandbox::endpoint::CAPABILITY,
    ),
    ("tool.Endpoint", tool_endpoint::CAPABILITY),
];

const HOST: (&str, &str) = ("host", flowcraft_kernel::handles::contracts::ENDPOINT_HOST);
const GRANTS: (&str, &str) = ("grants", flowcraft_kernel::handles::contracts::GRANTS);
const ENDPOINTS: (&str, &str) = (
    "endpoints",
    flowcraft_kernel::handles::contracts::ENDPOINT_PROVIDER,
);

/// The platform's builtin capability providers.
#[must_use]
pub fn registry() -> Registry {
    let mut registry = Registry::new();
    // Registration failures would mean a duplicate kind in this function.
    for outcome in [
        registry.register(local::LocalFactory),
        registry.register(scoped::ScopedFactory),
        registry.register(sub::SubFactory),
        registry.register(endpoint::EndpointFactory),
        registry.register(remote::RemoteFactory),
        registry.register(secret_memory::MemoryFactory),
        registry.register(secret_env::EnvFactory),
        registry.register(secret_file::FileFactory),
        registry.register(secret_endpoint::EndpointFactory),
        registry.register(secret_remote::RemoteFactory),
        registry.register(secret_keychain::KeychainFactory),
        registry.register(sandbox_local::LocalFactory),
        registry.register(flowcraft_cap_sandbox::BwrapFactory),
        registry.register(flowcraft_cap_sandbox::SeatbeltFactory),
        registry.register(sandbox_endpoint::EndpointFactory),
        registry.register(sandbox_remote::RemoteFactory),
        registry.register(tool_endpoint::EndpointFactory),
        registry.register(tool_remote::RemoteFactory),
        registry.register(flowcraft_cap_tool::mcp::Factory),
        registry.register(flowcraft_cap_tool::resource::RegistryFactory),
    ] {
        if let Err(error) = outcome {
            panic!("builtin registry is inconsistent: {error}");
        }
    }
    registry
}

/// Add the host-owned dependencies a platform endpoint needs, and produce the
/// externals that satisfy them.
pub fn prepare(platform: &Platform, deployment: &mut Deployment) -> Result<Vec<ExternalResource>> {
    let mut needs = [false; 3];
    for (name, resource) in &mut deployment.resources {
        if resource.kind.as_str().ends_with(".Endpoint") {
            resource.deps.entry("host".to_owned()).or_insert(
                Ref::new("host").map_err(|error| {
                    Error::new(error.kind(), format!("resources.{name}: {error}"))
                })?,
            );
            resource.deps.entry("grants".to_owned()).or_insert(
                Ref::new("grants").map_err(|error| {
                    Error::new(error.kind(), format!("resources.{name}: {error}"))
                })?,
            );
        }
        for key in resource.deps.keys() {
            match key.as_str() {
                "host" => needs[0] = true,
                "grants" => needs[1] = true,
                "endpoints" => needs[2] = true,
                _ => {}
            }
        }
    }
    for declared in &deployment.externals {
        match declared.name.as_str() {
            "host" => needs[0] = true,
            "grants" => needs[1] = true,
            "endpoints" => needs[2] = true,
            _ => {}
        }
    }

    let mut externals = Vec::new();
    if needs[0] {
        // The platform's host carries the bootstrap secret provider, so TLS
        // material resolves against the same source as the client side.
        externals.push(ExternalResource::new(
            "host",
            HOST.1,
            platform.endpoint_host(),
        ));
    }
    if needs[1] {
        let grants: Grants = platform.grants();
        externals.push(ExternalResource::new("grants", GRANTS.1, Arc::new(grants)));
    }
    if needs[2] {
        let endpoints: Endpoints = platform.endpoints();
        externals.push(ExternalResource::new(
            "endpoints",
            ENDPOINTS.1,
            Arc::new(endpoints),
        ));
    }
    Ok(externals)
}

/// Host-level checks the resource kernel cannot do: every `settings.endpoint`
/// must resolve, and an endpoint resource must serve the capability it claims.
///
/// This is what makes `check` honest — without it a typo in an endpoint name
/// passes the preflight and fails at build time, with the daemon already up.
#[must_use]
pub fn validate(platform: &Platform, deployment: &Deployment) -> Vec<PlanError> {
    let mut errors = Vec::new();
    for (name, resource) in &deployment.resources {
        let Some(settings) = &resource.settings else {
            continue;
        };
        let Some(endpoint) = settings.get("endpoint").and_then(|value| value.as_str()) else {
            continue;
        };
        let descriptor = match platform.resolve(endpoint) {
            Ok(descriptor) => descriptor,
            Err(error) => {
                errors.push(PlanError {
                    path: format!("resources.{name}.settings.endpoint"),
                    message: error.to_string(),
                });
                continue;
            }
        };
        let Some((_, expected)) = ENDPOINT_CAPABILITIES
            .iter()
            .find(|(kind, _)| *kind == resource.kind.as_str())
        else {
            continue;
        };
        let capability = settings
            .get("capability")
            .and_then(|value| value.as_str())
            .unwrap_or(expected);
        if !descriptor.capabilities.is_empty()
            && !descriptor
                .capabilities
                .iter()
                .any(|served| served == capability)
        {
            errors.push(PlanError {
                path: format!("resources.{name}.settings.capability"),
                message: format!("endpoint {endpoint:?} does not serve {capability:?}"),
            });
        }
    }
    errors
}

/// Options for [`build_with`].
#[derive(Default)]
pub struct BuildOptions {
    /// Stores the process owns rather than the document (an app's keychain, say).
    pub injected: InjectedSecrets,
}

impl std::fmt::Debug for BuildOptions {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("BuildOptions")
            .field("injected", &self.injected)
            .finish()
    }
}

impl BuildOptions {
    /// Inject a store the process owns as the target of `${secret:NAME}`.
    #[must_use]
    pub fn with_default_secret(
        mut self,
        id: impl Into<String>,
        store: Arc<dyn flowcraft_resource::resolver::SecretStore>,
    ) -> Self {
        self.injected = self.injected.with_default(id, store);
        self
    }
}

/// Plan, construct, and start a deployment.
pub async fn build(
    platform: &Platform,
    deployment: Deployment,
    base_dir: Option<&Path>,
) -> Result<Built> {
    build_with(platform, deployment, base_dir, &BuildOptions::default()).await
}

/// Plan, construct, and start a deployment, with host-injected secret stores.
///
/// `${secret:...}` needs a store that is itself built by this deployment, so the
/// observer attaches every `secret.Store` as soon as it exists and the resolver
/// sees it from then on. A store that cannot be attached (a duplicate `id`, a
/// second `default: true`) fails the deployment after the graph is built, even
/// when no reference ever resolved through it.
pub async fn build_with(
    platform: &Platform,
    deployment: Deployment,
    base_dir: Option<&Path>,
    options: &BuildOptions,
) -> Result<Built> {
    let mut document = deployment;
    let host_errors = validate(platform, &document);
    if !host_errors.is_empty() {
        return Err(Error::validation(render_plan_errors(&host_errors)));
    }
    let externals = prepare(platform, &mut document)?;

    let secrets = DeploymentSecrets::new();
    secrets.attach_injected(&options.injected)?;
    let mut builder = Builder::new(Arc::new(registry()))
        .with_externals(externals)
        .with_observer(secrets.observer())
        .with_resolver(Resolver::new().with_table(secrets.table()));
    if let Some(dir) = base_dir {
        builder = builder.with_base_dir(PathBuf::from(dir));
    }
    let built = builder.deploy(document).await?;
    if let Err(error) = secrets.check() {
        let _ = built.shutdown().await;
        return Err(error);
    }
    if let Err(error) = built.start().await {
        // The graph exists but is not usable; close it before reporting, so a
        // failed reload cannot leave listeners behind.
        let _ = built.shutdown().await;
        return Err(error);
    }
    Ok(built)
}

fn render_plan_errors(errors: &[PlanError]) -> String {
    let mut message = String::from("deployment validation failed:");
    for error in errors {
        message.push_str("\n  ");
        if error.path.is_empty() {
            message.push_str(&error.message);
        } else {
            message.push_str(&format!("{}: {}", error.path, error.message));
        }
    }
    message
}

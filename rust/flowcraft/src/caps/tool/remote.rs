//! The remote client: another process's tool source presented as the local
//! contract.

use std::collections::BTreeSet;
use std::sync::Arc;

use crate::client::grpc::from_status;
use crate::client::wire::Wire;
use crate::error::{Error, Result};
use crate::kernel::{EndpointProvider, Endpoints};
use crate::proto::message as msg;
use crate::proto::tool::{ExecuteRequest, ListRequest, tool_source_client::ToolSourceClient};
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Spec, require};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::Value;

use crate::caps::tool::contract::{
    CONTRACT, Handle, KIND, ToolSource, validate_content, validate_definition,
};
use crate::caps::tool::scope::Scope;

/// The implementation name.
pub const IMPL: &str = "remote";

/// A tool source served by another process.
pub struct RemoteSource {
    wire: Wire,
}

impl std::fmt::Debug for RemoteSource {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RemoteSource")
            .field("wire", &self.wire)
            .finish_non_exhaustive()
    }
}

impl RemoteSource {
    /// Mint a grant and prepare a client. No I/O until the first call.
    pub async fn connect(
        provider: Arc<dyn EndpointProvider>,
        endpoint: &str,
        tenant: &str,
        scope: Value,
    ) -> Result<Self> {
        Ok(Self {
            wire: Wire::open(provider, endpoint, CONTRACT, tenant, scope).await?,
        })
    }
}

#[async_trait]
impl ToolSource for RemoteSource {
    async fn list(&self) -> Result<Vec<msg::ToolDefinition>> {
        let mut client = ToolSourceClient::new(self.wire.channel().await?);
        let request = self.wire.request(ListRequest {}).await?;
        let response = client.list(request).await.map_err(from_status)?;
        let mut definitions = Vec::new();
        let mut seen = BTreeSet::new();
        for descriptor in response.into_inner().tools {
            let definition = descriptor.definition.ok_or_else(|| {
                Error::validation(format!(
                    "remote tool {:?} carries no definition",
                    descriptor.name
                ))
            })?;
            if definition.name != descriptor.name {
                return Err(Error::validation(format!(
                    "remote tool {:?} describes itself as {:?}",
                    descriptor.name, definition.name
                )));
            }
            validate_definition(&definition)?;
            if !seen.insert(definition.name.clone()) {
                return Err(Error::internal(format!(
                    "remote tool source listed {:?} twice",
                    definition.name
                )));
            }
            definitions.push(definition);
        }
        Ok(definitions)
    }

    async fn execute(&self, name: &str, arguments: &str) -> Result<msg::Content> {
        let mut client = ToolSourceClient::new(self.wire.channel().await?);
        let request = self
            .wire
            .request(ExecuteRequest {
                name: name.to_owned(),
                arguments: arguments.to_owned(),
            })
            .await?;
        let response = client.execute(request).await.map_err(from_status)?;
        let content = response
            .into_inner()
            .content
            .ok_or_else(|| Error::internal("remote tool returned no content"))?;
        validate_content(&content)?;
        Ok(content)
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    endpoint: String,
    #[serde(default)]
    tenant: String,
    #[serde(default)]
    scope: Value,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
    if settings.endpoint.trim().is_empty() {
        return Err(Error::validation("settings.endpoint is required"));
    }
    Scope::parse(&settings.scope)?;
    Ok(())
}

/// The `tool.Source/remote` factory.
pub struct RemoteFactory;

impl Factory for RemoteFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut endpoints = require::<Endpoints>("endpoints");
        endpoints.contract = Some(crate::kernel::handles::contracts::ENDPOINT_PROVIDER);
        Spec {
            contract: Some(CONTRACT),
            imp: Some(IMPL),
            deps: vec![endpoints],
            phase: Phase::Main,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let endpoints = ctx.deps.get::<Endpoints>("endpoints")?;
            let scope = if settings.scope.is_null() {
                serde_json::json!({})
            } else {
                settings.scope
            };
            let source = RemoteSource::connect(
                endpoints.0.clone(),
                &settings.endpoint,
                &settings.tenant,
                scope,
            )
            .await?;
            Ok(Arc::new(Handle::new(Arc::new(source))))
        })
    }
}

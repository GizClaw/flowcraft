//! The MCP tool source: adapt an external MCP server into the local contract.
//!
//! Ported from `core/tool/mcp`: one resource can attach several servers (stdio
//! or streamable HTTP), qualify tool names with `<server>__`, and map MCP
//! content blocks onto `flowcraft.message.v1` parts. The mapping is deliberately
//! lossless where it has a typed home and falls back to a JSON data part
//! otherwise.
//!
//! Connection is lazy: a required server is awaited for up to `connect_timeout`
//! (default 30s) during build, while an optional one connects in the background
//! and publishes its tools through the registry's [`Registrar`]. A tool whose
//! server has not connected yet connects on first use.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::{Arc, Weak};
use std::time::Duration;

use crate::error::{Error, ErrorKind, Result};
use crate::proto::message as msg;
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Phase, Spec};
use async_trait::async_trait;
use base64::Engine;
use base64::engine::general_purpose::STANDARD as BASE64;
use rmcp::ServiceExt;
use rmcp::model::{
    CallToolRequestParams, CallToolResult, ClientRequest, ContentBlock, ListResourcesResult,
    PingRequest, ProtocolVersion, ReadResourceRequestParams, ReadResourceResult, ResourceContents,
    Tool as McpToolInfo,
};
use rmcp::service::{RoleClient, RunningService};
use rmcp::transport::StreamableHttpClientTransport;
use rmcp::transport::TokioChildProcess;
use rmcp::transport::streamable_http_client::StreamableHttpClientTransportConfig;
use serde::Deserialize;
use serde_json::{Map, Value};

use crate::caps::tool::callable::{CallContext, Tool};
use crate::caps::tool::contract::{CONTRACT, Handle, ToolSource};
use crate::caps::tool::registry::{Registrar, Source};
use crate::message::{text_content, validate_content, validate_definition};

/// The implementation name.
pub const IMPL: &str = "mcp";

/// The stdio transport.
pub const TRANSPORT_STDIO: &str = "stdio";
/// The streamable-HTTP transport.
pub const TRANSPORT_HTTP: &str = "http";

/// The separator between a server name and a tool name.
pub const PREFIX_SEPARATOR: &str = "__";

/// The resource settings: one or more MCP servers.
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Settings {
    /// Servers to attach, in order.
    pub servers: Vec<ServerSpec>,
}

/// One MCP server attachment.
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ServerSpec {
    /// Logical server name; also the default tool-name prefix.
    pub name: String,
    /// `stdio` or `http`.
    pub transport: String,
    /// The command to spawn (stdio).
    #[serde(default)]
    pub command: String,
    /// Command arguments (stdio).
    #[serde(default)]
    pub args: Vec<String>,
    /// Extra environment variables (stdio).
    #[serde(default)]
    pub env: BTreeMap<String, String>,
    /// The server URL (http).
    #[serde(default)]
    pub url: String,
    /// Extra request headers (http).
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
    /// Override the `<server>__` prefix. An empty prefix disables namespacing.
    #[serde(default)]
    pub prefix: Option<String>,
    /// An unreachable optional server does not fail the whole source.
    #[serde(default = "default_true")]
    pub required: bool,
    /// Bridge the server's MCP resources into list/read tools.
    #[serde(default)]
    pub resources: bool,
    /// Per-call deadline, e.g. `30s`.
    #[serde(default)]
    pub http_timeout: Option<String>,
    /// Liveness probe interval: `off`, or a duration (default `15s`). Peers on
    /// protocol 2026-07-28+ are never pinged (SEP-2575 removed the method).
    #[serde(default)]
    pub liveness: Option<String>,
    /// How long a required server may take to connect during build (default
    /// `30s`). Optional servers never block.
    #[serde(default)]
    pub connect_timeout: Option<String>,
    /// Allow a plaintext (`http://` / `ws://`) URL to a **loopback** host.
    ///
    /// Off by default, and refused for anything that is not loopback: a remote
    /// target carries grants and tool results, and this used to be checked only
    /// for `platform.yaml` targets — a capability's settings never reached that
    /// rule at all (`S7`).
    #[serde(default)]
    pub allow_insecure: bool,
    /// Hand the daemon's whole environment to a stdio server.
    ///
    /// Off by default: a spawned MCP server is a separate program with whatever
    /// its author wrote in it, and inheriting the daemon's environment hands it
    /// every credential the deployment happened to export. `env` above is the
    /// allow-list that replaces it.
    #[serde(default)]
    pub inherit_host_env: bool,
}

fn default_true() -> bool {
    true
}

/// The default liveness interval, matching Go's `DefaultLivenessInterval`.
const DEFAULT_LIVENESS: Duration = Duration::from_secs(15);

/// The default wait for a required server's initial connection.
const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(30);

fn parse_connect_timeout(raw: Option<&str>) -> Result<Duration> {
    let Some(raw) = raw else {
        return Ok(DEFAULT_CONNECT_TIMEOUT);
    };
    let value = crate::caps::tool::middleware::parse_duration(raw)?;
    if value.is_zero() {
        return Err(Error::validation(
            "mcp: connect_timeout must be a positive duration",
        ));
    }
    Ok(value)
}

/// Parse a per-server liveness setting: `off` disables probing; anything else
/// must be a positive duration. Absent keeps the default.
fn parse_liveness(raw: Option<&str>) -> Result<Option<Duration>> {
    let Some(raw) = raw else {
        return Ok(Some(DEFAULT_LIVENESS));
    };
    if raw.trim().eq_ignore_ascii_case("off") {
        return Ok(None);
    }
    let interval = crate::caps::tool::middleware::parse_duration(raw).map_err(|error| {
        Error::validation(format!(
            "mcp: liveness must be \"off\" or a positive duration: {error}"
        ))
    })?;
    if interval.is_zero() {
        return Err(Error::validation(
            "mcp: liveness must be \"off\" or a positive duration",
        ));
    }
    Ok(Some(interval))
}

impl Settings {
    /// Check the spec's invariants.
    pub fn validate(&self) -> Result<()> {
        if self.servers.is_empty() {
            return Err(Error::validation(
                "mcp: settings must declare at least one server",
            ));
        }
        let mut seen = BTreeSet::new();
        for (index, server) in self.servers.iter().enumerate() {
            if server.name.trim().is_empty() {
                return Err(Error::validation(format!(
                    "mcp: servers[{index}]: name is required"
                )));
            }
            if !seen.insert(server.name.clone()) {
                return Err(Error::validation(format!(
                    "mcp: servers[{index}]: duplicate name {:?}",
                    server.name
                )));
            }
            server.validate(index)?;
        }
        Ok(())
    }
}

impl ServerSpec {
    fn validate(&self, index: usize) -> Result<()> {
        match self.transport.as_str() {
            TRANSPORT_STDIO => {
                if self.command.is_empty() {
                    return Err(Error::validation(format!(
                        "mcp: servers[{index}] ({}): stdio requires a command",
                        self.name
                    )));
                }
                if !self.url.is_empty() || !self.headers.is_empty() {
                    return Err(Error::validation(format!(
                        "mcp: servers[{index}] ({}): url/headers are http fields, not stdio",
                        self.name
                    )));
                }
            }
            TRANSPORT_HTTP => {
                if self.url.is_empty() {
                    return Err(Error::validation(format!(
                        "mcp: servers[{index}] ({}): http requires a url",
                        self.name
                    )));
                }
                if !self.command.is_empty() || !self.args.is_empty() || !self.env.is_empty() {
                    return Err(Error::validation(format!(
                        "mcp: servers[{index}] ({}): command/args/env are stdio fields, not http",
                        self.name
                    )));
                }
                // The same rule `platform.yaml` targets follow, in one place:
                // `crate::net`. It lives below the kernel so a provider shape
                // (`tool` without `kernel`) can still enforce it.
                crate::net::require_secure_target(&self.url, self.allow_insecure).map_err(
                    |error| {
                        Error::validation(format!(
                            "mcp: servers[{index}] ({}): {}",
                            self.name,
                            error.message()
                        ))
                    },
                )?;
                if self.inherit_host_env {
                    return Err(Error::validation(format!(
                        "mcp: servers[{index}] ({}): inherit_host_env is a stdio setting, not http",
                        self.name
                    )));
                }
                // A header name is later parsed by `http`; refusing it here means
                // a bad name fails the build instead of the first call.
                for name in self.headers.keys() {
                    http::HeaderName::from_bytes(name.as_bytes()).map_err(|error| {
                        Error::validation(format!(
                            "mcp: servers[{index}] ({}): invalid header name {name:?}: {error}",
                            self.name
                        ))
                    })?;
                }
            }
            "" => {
                return Err(Error::validation(format!(
                    "mcp: servers[{index}] ({}): transport is required ({TRANSPORT_STDIO:?} or {TRANSPORT_HTTP:?})",
                    self.name
                )));
            }
            other => {
                return Err(Error::validation(format!(
                    "mcp: servers[{index}] ({}): unknown transport {other:?}",
                    self.name
                )));
            }
        }
        if let Some(timeout) = &self.http_timeout {
            crate::caps::tool::middleware::parse_duration(timeout)?;
        }
        parse_liveness(self.liveness.as_deref())?;
        parse_connect_timeout(self.connect_timeout.as_deref())?;
        Ok(())
    }

    fn prefix(&self) -> String {
        self.prefix
            .clone()
            .unwrap_or_else(|| format!("{}{PREFIX_SEPARATOR}", self.name))
    }
}

fn validate_settings(value: &Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
    settings.validate()
}

/// The `tool.Source/mcp` factory.
pub struct Factory;

impl Factory {
    /// Build the factory's spec, for registration tests.
    #[must_use]
    pub fn spec() -> Spec {
        let kind = Kind::new(crate::caps::tool::contract::KIND).unwrap_or_else(|error| {
            panic!("{} is invalid: {error}", crate::caps::tool::contract::KIND)
        });
        Spec {
            contract: Some(CONTRACT),
            imp: Some(IMPL),
            phase: Phase::Main,
            validate_settings: Some(validate_settings),
            ..Spec::new(kind)
        }
    }
}

impl crate::resource::Factory for Factory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        Self::spec()
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            settings.validate()?;
            let source = McpSource::connect(&settings).await?;
            Ok(Arc::new(Handle::new(Arc::new(source))))
        })
    }
}

/// A group of connected MCP servers, exposed as one tool source.
pub struct McpSource {
    servers: Vec<Arc<Server>>,
}

impl std::fmt::Debug for McpSource {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("McpSource")
            .field("tools", &self.len())
            .finish()
    }
}

impl McpSource {
    /// Start every declared server. Required servers are awaited up to their
    /// `connect_timeout`; optional servers connect in the background and publish
    /// their tools through the registry's [`Registrar`] once connected.
    pub async fn connect(settings: &Settings) -> Result<Self> {
        settings.validate()?;
        let mut servers = Vec::new();
        for spec in &settings.servers {
            let timeout = spec
                .http_timeout
                .as_deref()
                .map(crate::caps::tool::middleware::parse_duration)
                .transpose()?;
            let liveness = parse_liveness(spec.liveness.as_deref())?;
            let server = Arc::new(Server::new(spec.clone(), spec.prefix(), timeout));
            if let Some(interval) = liveness {
                spawn_liveness(Arc::downgrade(&server), interval);
            }
            let task = {
                let server = server.clone();
                tokio::spawn(async move { server.ensure_service().await.map(|_| ()) })
            };
            if spec.required {
                let limit = parse_connect_timeout(spec.connect_timeout.as_deref())?;
                match tokio::time::timeout(limit, task).await {
                    Ok(Ok(Ok(()))) => {}
                    Ok(Ok(Err(error))) => return Err(error),
                    Ok(Err(error)) => {
                        return Err(Error::internal(format!(
                            "mcp: server {:?}: connect task: {error}",
                            spec.name
                        )));
                    }
                    Err(_) => {
                        return Err(crate::caps::tool::codes::timeout(
                            crate::caps::tool::codes::Code::Timeout,
                            format!("mcp: server {:?}: connect exceeded {limit:?}", spec.name),
                        ));
                    }
                }
            }
            servers.push(server);
        }
        Ok(Self { servers })
    }

    /// The number of tools currently known across servers.
    #[must_use]
    pub fn len(&self) -> usize {
        self.servers.iter().map(|server| server.tool_count()).sum()
    }

    /// Whether no server has published a tool yet.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

/// One configured server, shared by its tools.
struct Server {
    name: String,
    spec: ServerSpec,
    prefix: String,
    timeout: Option<Duration>,
    service: tokio::sync::Mutex<Option<Arc<RunningService<RoleClient, ()>>>>,
    connect: tokio::sync::Mutex<()>,
    tools: std::sync::Mutex<Vec<Arc<AdaptedTool>>>,
    known: std::sync::Mutex<BTreeSet<String>>,
    registrar: std::sync::Mutex<Option<Arc<dyn Registrar>>>,
}

impl Server {
    fn new(spec: ServerSpec, prefix: String, timeout: Option<Duration>) -> Self {
        Self {
            name: spec.name.clone(),
            spec,
            prefix,
            timeout,
            service: tokio::sync::Mutex::new(None),
            connect: tokio::sync::Mutex::new(()),
            tools: std::sync::Mutex::new(Vec::new()),
            known: std::sync::Mutex::new(BTreeSet::new()),
            registrar: std::sync::Mutex::new(None),
        }
    }

    fn tool_count(&self) -> usize {
        self.tools.lock().map(|tools| tools.len()).unwrap_or(0)
    }

    fn tool_snapshot(&self) -> Vec<Arc<AdaptedTool>> {
        self.tools
            .lock()
            .map(|tools| tools.clone())
            .unwrap_or_default()
    }

    async fn within<F, T>(&self, future: F) -> Result<T>
    where
        F: std::future::Future<Output = std::result::Result<T, rmcp::ServiceError>>,
    {
        let wrap = |error: rmcp::ServiceError| {
            Error::not_available(format!("mcp: server {:?}: {error}", self.name))
        };
        match self.timeout {
            Some(limit) => match tokio::time::timeout(limit, future).await {
                Ok(result) => result.map_err(wrap),
                Err(_) => Err(crate::caps::tool::codes::timeout(
                    crate::caps::tool::codes::Code::Timeout,
                    format!("mcp: server {:?}: exceeded {limit:?}", self.name),
                )),
            },
            None => future.await.map_err(wrap),
        }
    }

    /// Connect on first use. Serialized so concurrent tool calls share one
    /// connection attempt.
    async fn ensure_service(self: &Arc<Self>) -> Result<Arc<RunningService<RoleClient, ()>>> {
        if let Some(service) = self.service.lock().await.clone() {
            return Ok(service);
        }
        let _guard = self.connect.lock().await;
        if let Some(service) = self.service.lock().await.clone() {
            return Ok(service);
        }
        let service = Arc::new(connect_server(&self.spec).await?);
        *self.service.lock().await = Some(service.clone());
        self.load_tools(&service).await?;
        Ok(service)
    }

    async fn load_tools(
        self: &Arc<Self>,
        service: &Arc<RunningService<RoleClient, ()>>,
    ) -> Result<Vec<Arc<AdaptedTool>>> {
        let listed = self.within(service.list_tools(None)).await?;
        let mut tools = Vec::new();
        for info in &listed.tools {
            tools.push(call_tool(self, info)?);
        }
        if self.spec.resources {
            tools.extend(resource_tools(self)?);
        }
        if let Ok(mut slot) = self.tools.lock() {
            *slot = tools.clone();
        }
        self.publish(&tools);
        Ok(tools)
    }

    /// Publish the current tool set through the registrar, when one is
    /// attached. Called on first connect, on attach, and after a reconnect.
    fn publish(self: &Arc<Self>, tools: &[Arc<AdaptedTool>]) {
        let registrar = self.registrar.lock().ok().and_then(|slot| slot.clone());
        let mut next = BTreeSet::new();
        for tool in tools {
            next.insert(tool.definition.name.clone());
            if let Some(registrar) = &registrar {
                registrar.remove(&tool.definition.name);
                if let Err(error) = registrar.add(tool.clone()) {
                    tracing::warn!(
                        server = %self.name,
                        tool = %tool.definition.name,
                        %error,
                        "mcp: publish tool failed"
                    );
                }
            }
        }
        let stale: Vec<String> = {
            let Ok(known) = self.known.lock() else {
                return;
            };
            known.difference(&next).cloned().collect()
        };
        if let Some(registrar) = &registrar {
            for name in stale {
                registrar.remove(&name);
            }
        }
        if let Ok(mut known) = self.known.lock() {
            *known = next;
        }
    }

    async fn call_tool(
        self: &Arc<Self>,
        remote: String,
        arguments: Map<String, Value>,
    ) -> Result<CallToolResult> {
        let service = self.ensure_service().await?;
        let first = self
            .within(service.call_tool(
                CallToolRequestParams::new(remote.clone()).with_arguments(arguments.clone()),
            ))
            .await;
        match first {
            Ok(result) => Ok(result),
            Err(error) if error.kind() == ErrorKind::NotAvailable => {
                let service = self.reconnect().await?;
                self.within(
                    service.call_tool(CallToolRequestParams::new(remote).with_arguments(arguments)),
                )
                .await
            }
            Err(error) => Err(error),
        }
    }

    async fn list_resources(self: &Arc<Self>) -> Result<ListResourcesResult> {
        let service = self.ensure_service().await?;
        match self.within(service.list_resources(None)).await {
            Ok(result) => Ok(result),
            Err(error) if error.kind() == ErrorKind::NotAvailable => {
                let service = self.reconnect().await?;
                self.within(service.list_resources(None)).await
            }
            Err(error) => Err(error),
        }
    }

    async fn read_resource(self: &Arc<Self>, uri: String) -> Result<ReadResourceResult> {
        let service = self.ensure_service().await?;
        let first = self
            .within(service.read_resource(ReadResourceRequestParams::new(uri.clone())))
            .await;
        match first {
            Ok(result) => Ok(result),
            Err(error) if error.kind() == ErrorKind::NotAvailable => {
                let service = self.reconnect().await?;
                self.within(service.read_resource(ReadResourceRequestParams::new(uri)))
                    .await
            }
            Err(error) => Err(error),
        }
    }

    async fn reconnect(self: &Arc<Self>) -> Result<Arc<RunningService<RoleClient, ()>>> {
        *self.service.lock().await = None;
        self.ensure_service().await
    }
}

async fn connect_server(spec: &ServerSpec) -> Result<RunningService<RoleClient, ()>> {
    match spec.transport.as_str() {
        TRANSPORT_STDIO => {
            let mut command = tokio::process::Command::new(&spec.command);
            command.args(&spec.args);
            // `env_clear` unless the deployment asked otherwise: a stdio server
            // is a third-party program, and an inherited environment hands it
            // every credential the daemon was started with.
            if !spec.inherit_host_env {
                command.env_clear();
            }
            for (name, value) in &spec.env {
                command.env(name, value);
            }
            let transport = TokioChildProcess::new(command).map_err(|error| {
                Error::internal(format!(
                    "mcp: server {:?}: spawn {:?}: {error}",
                    spec.name, spec.command
                ))
            })?;
            ().serve(transport).await.map_err(|error| {
                Error::not_available(format!("mcp: server {:?}: initialize: {error}", spec.name))
            })
        }
        TRANSPORT_HTTP => {
            let mut config = StreamableHttpClientTransportConfig::with_uri(spec.url.as_str());
            for (name, value) in &spec.headers {
                let name = http::HeaderName::from_bytes(name.as_bytes()).map_err(|error| {
                    Error::validation(format!(
                        "mcp: server {:?}: invalid header name: {error}",
                        spec.name
                    ))
                })?;
                let value = http::HeaderValue::from_str(value).map_err(|error| {
                    Error::validation(format!(
                        "mcp: server {:?}: invalid header value: {error}",
                        spec.name
                    ))
                })?;
                config.custom_headers.insert(name, value);
            }
            let transport = StreamableHttpClientTransport::from_config(config);
            ().serve(transport).await.map_err(|error| {
                Error::not_available(format!("mcp: server {:?}: initialize: {error}", spec.name))
            })
        }
        other => Err(Error::validation(format!(
            "mcp: server {:?}: unknown transport {other:?}",
            spec.name
        ))),
    }
}

fn describe(info: &McpToolInfo) -> String {
    let title = info.title.as_deref().unwrap_or_default();
    let description = info.description.as_deref().unwrap_or_default();
    match (title.is_empty(), description.is_empty()) {
        (false, false) => format!("{title}: {description}"),
        (false, true) => title.to_owned(),
        (true, false) => description.to_owned(),
        (true, true) => String::new(),
    }
}

fn schema_bytes(info: &McpToolInfo) -> Vec<u8> {
    serde_json::to_vec(&*info.input_schema).unwrap_or_else(|_| b"{\"type\":\"object\"}".to_vec())
}

/// Probe a server periodically; a failed ping reconnects.
///
/// SEP-2575 removed `ping` in protocol 2026-07-28, so a modern peer is never
/// probed. Servers that have not connected yet are skipped: liveness never
/// initiates a connection on its own.
fn spawn_liveness(server: Weak<Server>, interval: Duration) {
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(interval);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        loop {
            ticker.tick().await;
            let Some(server) = server.upgrade() else {
                return;
            };
            let service = server.service.lock().await.clone();
            let Some(service) = service else {
                continue;
            };
            let modern = service
                .peer_info()
                .is_some_and(|info| info.protocol_version >= ProtocolVersion::V_2026_07_28);
            if modern {
                continue;
            }
            if let Err(error) = server
                .within(service.send_request(ClientRequest::PingRequest(PingRequest::default())))
                .await
            {
                tracing::warn!(server = %server.name, %error, "mcp: liveness ping failed");
                if let Err(error) = server.reconnect().await {
                    tracing::warn!(server = %server.name, %error, "mcp: reconnect after ping failure failed");
                }
            }
        }
    });
}

fn qualify(prefix: &str, name: &str) -> String {
    if prefix.is_empty() {
        name.to_owned()
    } else {
        format!("{prefix}{name}")
    }
}

fn call_tool(server: &Arc<Server>, info: &McpToolInfo) -> Result<Arc<AdaptedTool>> {
    let remote = info.name.to_string();
    let definition = msg::ToolDefinition {
        name: qualify(&server.prefix, &remote),
        description: describe(info),
        input_schema: schema_bytes(info),
    };
    validate_definition(&definition)?;
    Ok(Arc::new(AdaptedTool {
        server: server.clone(),
        definition,
        kind: ToolKind::Call { remote },
    }))
}

fn resource_tools(server: &Arc<Server>) -> Result<Vec<Arc<AdaptedTool>>> {
    let mut tools = Vec::new();
    for (suffix, description, kind) in [
        (
            "list_resources",
            "List the resources this MCP server exposes (uri, name, description, mime type, size).",
            ToolKind::ListResources,
        ),
        (
            "read_resource",
            "Read one resource from this MCP server by URI.",
            ToolKind::ReadResource,
        ),
    ] {
        let definition = msg::ToolDefinition {
            name: qualify(&server.prefix, suffix),
            description: description.to_owned(),
            input_schema: resource_schema(kind == ToolKind::ReadResource),
        };
        validate_definition(&definition)?;
        tools.push(Arc::new(AdaptedTool {
            server: server.clone(),
            definition,
            kind,
        }));
    }
    Ok(tools)
}

/// One MCP tool presented as a local [`Tool`].
struct AdaptedTool {
    server: Arc<Server>,
    definition: msg::ToolDefinition,
    kind: ToolKind,
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum ToolKind {
    Call { remote: String },
    ListResources,
    ReadResource,
}

fn resource_schema(read: bool) -> Vec<u8> {
    if read {
        serde_json::to_vec(&serde_json::json!({
            "type": "object",
            "properties": {
                "uri": {"type": "string", "description": "the resource URI to read"}
            },
            "required": ["uri"]
        }))
        .unwrap_or_else(|_| b"{\"type\":\"object\"}".to_vec())
    } else {
        b"{\"type\":\"object\"}".to_vec()
    }
}

#[async_trait]
impl Tool for AdaptedTool {
    fn definition(&self) -> msg::ToolDefinition {
        self.definition.clone()
    }

    async fn execute(&self, _context: &CallContext, arguments: &str) -> Result<msg::Content> {
        let arguments = parse_arguments(arguments)?;
        match &self.kind {
            ToolKind::Call { remote } => {
                let result = self.server.call_tool(remote.clone(), arguments).await?;
                if result.is_error == Some(true) {
                    let rendered = rendered(&result);
                    return Err(Error::internal(if rendered.trim().is_empty() {
                        format!("mcp tool {remote:?} reported an error with no detail")
                    } else {
                        rendered
                    }));
                }
                content_of(&result)
            }
            ToolKind::ListResources => {
                let listed = self.server.list_resources().await?;
                Ok(json_content(&serde_json::json!({
                    "resources": listed.resources,
                })))
            }
            ToolKind::ReadResource => {
                let Some(serde_json::Value::String(uri)) = arguments.get("uri") else {
                    return Err(Error::validation(
                        "mcp read_resource: arguments.uri is required",
                    ));
                };
                let result = self.server.read_resource(uri.clone()).await?;
                let mut parts = Vec::new();
                for contents in &result.contents {
                    if let Some(part) = resource_part(contents) {
                        parts.push(part);
                    }
                }
                if parts.is_empty() {
                    return Ok(text_content(""));
                }
                let content = msg::Content { parts };
                validate_content(&content)?;
                Ok(content)
            }
        }
    }
}

fn json_content(value: &serde_json::Value) -> msg::Content {
    msg::Content {
        parts: vec![json_part(value)],
    }
}

#[async_trait]
impl Source for McpSource {
    async fn tools(&self) -> Result<Vec<Arc<dyn Tool>>> {
        Ok(self
            .servers
            .iter()
            .flat_map(|server| server.tool_snapshot())
            .map(|tool| tool as Arc<dyn Tool>)
            .collect())
    }

    fn attach(&self, registrar: Arc<dyn Registrar>) {
        for server in &self.servers {
            if let Ok(mut slot) = server.registrar.lock() {
                *slot = Some(registrar.clone());
            }
            // Publish tools that connected before the registry attached.
            server.publish(&server.tool_snapshot());
        }
    }
}

#[async_trait]
impl ToolSource for McpSource {
    async fn list(&self) -> Result<Vec<msg::ToolDefinition>> {
        Ok(self
            .servers
            .iter()
            .flat_map(|server| server.tool_snapshot())
            .map(|tool| tool.definition())
            .collect())
    }

    async fn execute(&self, name: &str, arguments: &str) -> Result<msg::Content> {
        let tool = self
            .servers
            .iter()
            .flat_map(|server| server.tool_snapshot())
            .find(|tool| tool.definition.name == name)
            .ok_or_else(|| Error::not_found(format!("mcp: tool {name:?} is not registered")))?;
        tool.execute(&CallContext::default(), arguments).await
    }
}

fn parse_arguments(arguments: &str) -> Result<Map<String, Value>> {
    let trimmed = arguments.trim();
    if trimmed.is_empty() {
        return Ok(Map::new());
    }
    match serde_json::from_str::<Value>(trimmed)
        .map_err(|error| Error::validation(format!("mcp: parse arguments: {error}")))?
    {
        Value::Null => Ok(Map::new()),
        Value::Object(map) => Ok(map),
        _ => Err(crate::caps::tool::codes::validation(
            crate::caps::tool::codes::Code::Arguments,
            "mcp: arguments must be a JSON object",
        )),
    }
}

/// Map one call result onto canonical content.
fn content_of(result: &CallToolResult) -> Result<msg::Content> {
    let mut parts = Vec::new();
    for block in &result.content {
        if let Some(part) = block_part(block) {
            parts.push(part);
        }
    }
    if parts.is_empty()
        && let Some(structured) = &result.structured_content
    {
        parts.push(json_part(structured));
    }
    if parts.is_empty() {
        return Ok(text_content(""));
    }
    let content = msg::Content { parts };
    validate_content(&content)?;
    Ok(content)
}

fn rendered(result: &CallToolResult) -> String {
    let mut text = String::new();
    for block in &result.content {
        if let ContentBlock::Text(content) = block {
            text.push_str(&content.text);
        }
    }
    if text.is_empty()
        && let Some(structured) = &result.structured_content
    {
        text = structured.to_string();
    }
    text
}

fn block_part(block: &ContentBlock) -> Option<msg::Part> {
    let typed =
        match block {
            ContentBlock::Text(text) if !text.text.is_empty() => Some(text_part(&text.text)),
            ContentBlock::Image(image) => decode(&image.data)
                .map(|bytes| media_part(&image.mime_type, bytes, MediaKind::Image)),
            ContentBlock::Audio(audio) => decode(&audio.data)
                .map(|bytes| media_part(&audio.mime_type, bytes, MediaKind::Audio)),
            ContentBlock::ResourceLink(link) if !link.uri.is_empty() => Some(msg::Part {
                kind: Some(msg::part::Kind::File(msg::FilePart {
                    uri: link.uri.clone(),
                    media_type: link.mime_type.clone().unwrap_or_default(),
                    name: link.name.clone(),
                })),
            }),
            ContentBlock::Resource(embedded) => resource_part(&embedded.resource),
            _ => None,
        };
    // No typed home: keep the wire form rather than dropping the block.
    typed.or_else(|| {
        serde_json::to_value(block)
            .ok()
            .map(|value| json_part(&value))
    })
}

fn resource_part(contents: &ResourceContents) -> Option<msg::Part> {
    match contents {
        ResourceContents::TextResourceContents { text, .. } if !text.is_empty() => {
            Some(text_part(text))
        }
        ResourceContents::BlobResourceContents {
            blob, mime_type, ..
        } => {
            let media_type = mime_type.clone().unwrap_or_default();
            let bytes = decode(blob)?;
            if media_type.starts_with("image/") {
                Some(media_part(&media_type, bytes, MediaKind::Image))
            } else if media_type.starts_with("audio/") {
                Some(media_part(&media_type, bytes, MediaKind::Audio))
            } else if media_type.starts_with("video/") {
                Some(media_part(&media_type, bytes, MediaKind::Video))
            } else {
                None
            }
        }
        _ => None,
    }
}

enum MediaKind {
    Image,
    Audio,
    Video,
}

fn media_part(media_type: &str, bytes: Vec<u8>, kind: MediaKind) -> msg::Part {
    let source = msg::MediaSource {
        media_type: media_type.to_owned(),
        from: Some(msg::media_source::From::Data(bytes)),
    };
    let kind = match kind {
        MediaKind::Image => msg::part::Kind::Image(msg::ImagePart {
            source: Some(source),
        }),
        MediaKind::Audio => msg::part::Kind::Audio(msg::AudioPart {
            source: Some(source),
            format: None,
            duration_millis: None,
        }),
        MediaKind::Video => msg::part::Kind::Video(msg::VideoPart {
            source: Some(source),
        }),
    };
    msg::Part { kind: Some(kind) }
}

fn text_part(text: &str) -> msg::Part {
    msg::Part {
        kind: Some(msg::part::Kind::Text(msg::TextPart {
            text: text.to_owned(),
        })),
    }
}

fn json_part(value: &Value) -> msg::Part {
    if value.is_object() {
        msg::Part {
            kind: Some(msg::part::Kind::Data(msg::DataPart {
                media_type: "application/json".to_owned(),
                value: serde_json::to_vec(value).unwrap_or_default(),
            })),
        }
    } else {
        text_part(&value.to_string())
    }
}

fn decode(data: &str) -> Option<Vec<u8>> {
    BASE64.decode(data).ok()
}

#[cfg(test)]
mod tests {
    use super::*;
    use rmcp::model::TextContent;

    fn result_with(contents: Vec<ContentBlock>) -> CallToolResult {
        let mut result = CallToolResult::default();
        result.content = contents;
        result
    }

    fn decode_part(part: &msg::Part) -> Value {
        match part.kind.as_ref().expect("kind") {
            msg::part::Kind::Data(data) => serde_json::from_slice(&data.value).expect("json"),
            other => panic!("unexpected part {other:?}"),
        }
    }

    #[test]
    fn settings_require_a_valid_transport() {
        let settings: Settings = serde_json::from_value(serde_json::json!({
            "servers": [{"name": "fs", "transport": "stdio", "command": "server"}]
        }))
        .expect("settings");
        settings.validate().expect("valid");

        let settings: Settings = serde_json::from_value(serde_json::json!({
            "servers": [{"name": "fs", "transport": "http", "url": "https://x/mcp"}]
        }))
        .expect("settings");
        settings.validate().expect("valid");
    }

    /// The rule that was missing entirely: an `http://` MCP target used to be
    /// accepted, because the "a remote target requires tls" check only ever ran
    /// over `platform.yaml` (`S7`).
    #[test]
    fn a_plaintext_http_target_is_refused_unless_loopback_and_opted_in() {
        let refused = |url: &str, extra: serde_json::Value| {
            let mut server = serde_json::json!({"name": "x", "transport": "http", "url": url});
            if let Some(object) = extra.as_object() {
                for (key, value) in object {
                    server[key] = value.clone();
                }
            }
            let settings: Settings =
                serde_json::from_value(serde_json::json!({ "servers": [server] }))
                    .expect("settings");
            settings.validate().expect_err("refused").to_string()
        };

        let error = refused("http://api.example.com/mcp", serde_json::Value::Null);
        assert!(error.contains("requires tls"), "{error}");
        let error = refused(
            "http://api.example.com/mcp",
            serde_json::json!({"allow_insecure": true}),
        );
        assert!(error.contains("only applies to a loopback host"), "{error}");
        let error = refused("ftp://api.example.com/mcp", serde_json::Value::Null);
        assert!(error.contains("does not speak"), "{error}");

        // Loopback plus the opt-in is the one plaintext shape that is allowed.
        let settings: Settings = serde_json::from_value(serde_json::json!({
            "servers": [{
                "name": "x", "transport": "http",
                "url": "http://127.0.0.1:8080/mcp", "allow_insecure": true,
            }]
        }))
        .expect("settings");
        settings.validate().expect("loopback opt-in");
    }

    /// A header name is parsed by `http` at call time; a bad one fails the build.
    #[test]
    fn an_invalid_header_name_fails_validation() {
        let settings: Settings = serde_json::from_value(serde_json::json!({
            "servers": [{
                "name": "x", "transport": "http", "url": "https://x/mcp",
                "headers": {"not a header": "v"},
            }]
        }))
        .expect("settings");
        let error = settings.validate().expect_err("refused").to_string();
        assert!(error.contains("invalid header name"), "{error}");
    }

    /// `inherit_host_env` is stdio-only, and off by default.
    #[test]
    fn the_host_environment_is_not_inherited_by_default() {
        let settings: Settings = serde_json::from_value(serde_json::json!({
            "servers": [{"name": "fs", "transport": "stdio", "command": "server"}]
        }))
        .expect("settings");
        assert!(!settings.servers[0].inherit_host_env, "default off");
        assert!(!settings.servers[0].allow_insecure, "default off");

        let settings: Settings = serde_json::from_value(serde_json::json!({
            "servers": [{
                "name": "x", "transport": "http", "url": "https://x/mcp",
                "inherit_host_env": true,
            }]
        }))
        .expect("settings");
        let error = settings.validate().expect_err("refused").to_string();
        assert!(error.contains("stdio setting"), "{error}");
    }

    #[test]
    fn invalid_transports_and_duplicates_are_refused() {
        for (spec, needle) in [
            (
                serde_json::json!({"servers": [{"name": "fs", "transport": "stdio"}]}),
                "command",
            ),
            (
                serde_json::json!({"servers": [{"name": "fs", "transport": "http"}]}),
                "url",
            ),
            (
                serde_json::json!({"servers": [{"name": "fs", "transport": "sse"}]}),
                "unknown transport",
            ),
            (
                serde_json::json!({"servers": [
                    {"name": "fs", "transport": "stdio", "command": "a"},
                    {"name": "fs", "transport": "stdio", "command": "b"}
                ]}),
                "duplicate name",
            ),
        ] {
            let settings: Settings = serde_json::from_value(spec).expect("settings");
            let error = settings.validate().unwrap_err();
            assert!(error.to_string().contains(needle), "{error}");
        }
    }

    #[test]
    fn connect_timeout_defaults_and_parses() {
        assert_eq!(
            parse_connect_timeout(None).unwrap(),
            Duration::from_secs(30)
        );
        assert_eq!(
            parse_connect_timeout(Some("5s")).unwrap(),
            Duration::from_secs(5)
        );
        assert!(parse_connect_timeout(Some("0s")).is_err());
        assert!(parse_connect_timeout(Some("soon")).is_err());
    }

    #[tokio::test]
    async fn an_optional_server_does_not_block_the_build() {
        let settings: Settings = serde_json::from_value(serde_json::json!({
            "servers": [{
                "name": "files",
                "transport": "stdio",
                "command": "definitely-not-a-real-binary-flowcraft",
                "required": false
            }]
        }))
        .expect("settings");
        let source = McpSource::connect(&settings).await.expect("connect");
        assert!(source.is_empty());
    }

    #[tokio::test]
    async fn a_required_server_fails_the_build() {
        let settings: Settings = serde_json::from_value(serde_json::json!({
            "servers": [{
                "name": "files",
                "transport": "stdio",
                "command": "definitely-not-a-real-binary-flowcraft",
                "connect_timeout": "2s"
            }]
        }))
        .expect("settings");
        assert!(McpSource::connect(&settings).await.is_err());
    }

    #[test]
    fn liveness_defaults_on_and_parses_off() {
        assert_eq!(parse_liveness(None).unwrap(), Some(Duration::from_secs(15)));
        assert_eq!(parse_liveness(Some("off")).unwrap(), None);
        assert_eq!(
            parse_liveness(Some("2m")).unwrap(),
            Some(Duration::from_secs(120))
        );
        assert!(parse_liveness(Some("0s")).is_err());
        assert!(parse_liveness(Some("soon")).is_err());
    }

    #[test]
    fn a_server_is_required_by_default_and_has_no_resource_bridge() {
        let spec: ServerSpec = serde_json::from_value(serde_json::json!({
            "name": "fs", "transport": "stdio", "command": "server"
        }))
        .expect("spec");
        assert!(spec.required);
        assert!(!spec.resources);
        assert!(spec.http_timeout.is_none());
    }

    #[test]
    fn a_qualifier_namespaces_only_when_non_empty() {
        assert_eq!(qualify("fs__", "read"), "fs__read");
        assert_eq!(qualify("", "read"), "read");
    }

    #[test]
    fn an_invalid_timeout_is_refused() {
        let settings: Settings = serde_json::from_value(serde_json::json!({
            "servers": [{
                "name": "fs",
                "transport": "http",
                "url": "https://example/mcp",
                "http_timeout": "soon"
            }]
        }))
        .expect("settings");
        assert!(settings.validate().is_err());
    }

    #[test]
    fn a_default_prefix_namespaces_the_tool() {
        let spec: ServerSpec = serde_json::from_value(serde_json::json!({
            "name": "fs", "transport": "stdio", "command": "server"
        }))
        .expect("spec");
        assert_eq!(spec.prefix(), "fs__");
        let spec: ServerSpec = serde_json::from_value(serde_json::json!({
            "name": "fs", "transport": "stdio", "command": "server", "prefix": ""
        }))
        .expect("spec");
        assert_eq!(spec.prefix(), "");
    }

    #[test]
    fn text_and_structured_content_map_to_parts() {
        let result = result_with(vec![ContentBlock::Text(TextContent::new("hi"))]);
        let content = content_of(&result).expect("content");
        assert_eq!(content, text_content("hi"));

        let mut result = CallToolResult::default();
        result.structured_content = Some(serde_json::json!({"answer": 42}));
        let content = content_of(&result).expect("content");
        assert_eq!(decode_part(&content.parts[0])["answer"], 42);
    }

    #[test]
    fn an_empty_result_is_one_empty_text_part() {
        let content = content_of(&CallToolResult::default()).expect("content");
        assert_eq!(content, text_content(""));
    }

    #[test]
    fn an_image_block_becomes_an_inline_image() {
        let data = BASE64.encode(b"png");
        let result = result_with(vec![ContentBlock::Image(rmcp::model::ImageContent::new(
            data,
            "image/png",
        ))]);
        let content = content_of(&result).expect("content");
        let msg::part::Kind::Image(image) = content.parts[0].kind.as_ref().expect("kind") else {
            panic!("expected an image part");
        };
        let source = image.source.as_ref().expect("source");
        assert_eq!(source.media_type, "image/png");
        assert_eq!(
            source.from,
            Some(msg::media_source::From::Data(b"png".to_vec()))
        );
    }

    #[test]
    fn an_embedded_text_resource_becomes_text() {
        let result = result_with(vec![ContentBlock::Resource(
            rmcp::model::EmbeddedResource::new(ResourceContents::TextResourceContents {
                uri: "file:///a".to_owned(),
                mime_type: Some("text/plain".to_owned()),
                text: "body".to_owned(),
                meta: None,
            }),
        )]);
        let content = content_of(&result).expect("content");
        assert_eq!(content, text_content("body"));
    }

    #[test]
    fn arguments_accept_empty_null_and_objects() {
        assert!(parse_arguments("").unwrap().is_empty());
        assert!(parse_arguments("null").unwrap().is_empty());
        assert_eq!(parse_arguments(r#"{"a":1}"#).unwrap()["a"], 1);
        assert!(parse_arguments("[]").is_err());
        assert!(parse_arguments("{oops").is_err());
    }
}

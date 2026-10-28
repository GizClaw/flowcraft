//! The tool capability and its in-process framework.
//!
//! Two layers live here:
//!
//! * **Capability** — the `tool.ToolSource/v1` wire: [`contract`], [`local`],
//!   [`endpoint`], [`remote`], [`scope`], and the MCP provider ([`mcp`]).
//! * **Framework** — the agent-side catalog ported from `core/tool`:
//!   [`tool`], [`registry`], [`exposure`], [`state`], [`visibility`],
//!   [`session`], [`search`], [`tool_search`], [`executor`], [`assembly`],
//!   and the `tool.Registry` resource factory in [`resource`].
//!
//! The split is by concern, not by dependency: both layers share the same
//! `flowcraft.message.v1` types, so a tool's definition and a turn's content
//! cannot drift apart.
//!
//! There is deliberately no `tool.Source/local` document factory for arbitrary
//! code: a tool's behavior is code, not YAML. A program that hosts tools builds
//! a [`local::LocalSource`], registers handlers, and injects it as an external
//! resource with contract [`CONTRACT`].

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

pub mod assembly;
pub mod callable;
pub mod codes;
pub mod contract;
pub mod executor;
pub mod exposure;
pub mod local;
pub mod mcp;
pub mod middleware;
pub mod registry;
pub mod resource;
pub mod search;
pub mod session;
pub mod state;
pub mod tool_search;
pub mod visibility;

// The endpoint server, the remote client, and the grant-scope checks they share.
#[cfg(feature = "wire")]
pub mod endpoint;
#[cfg(feature = "wire")]
pub mod remote;
#[cfg(feature = "wire")]
pub mod scope;

pub use self::callable::{
    CallContext, FuncTool, Handler as ToolHandler, Tool, ToolMeta, text_tool,
};
pub use assembly::{Assembly, Options as AssemblyOptions};
pub use contract::{CONTRACT, Handle, KIND, ToolSource};
pub use executor::{Dispatch, Dispatcher, Executor, Middleware};
pub use exposure::{Budget, DiscoveryPolicy, Exposure, Policy};
pub use local::{LocalSource, Tool as LocalTool};
pub use mcp::{McpSource, ServerSpec, Settings as McpSettings};
pub use registry::{Catalog, ConflictPolicy, Registry, Source, StaticSource};
#[cfg(feature = "wire")]
pub use remote::RemoteSource;
pub use resource::{HandleSource, RegistryFactory, RegistrySource};
pub use search::SearchHit;
pub use session::{
    DiscoverOutcome, DiscoverReason, DiscoverResult, DynamicSession, Session, StaticSession,
};
pub use tool_search::SearchTool;

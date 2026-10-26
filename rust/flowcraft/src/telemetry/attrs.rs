//! The field-name vocabulary.
//!
//! One spelling per fact, everywhere: logs, spans, the on-disk file, and the
//! support bundle all use these keys, so a query written against one is valid
//! against the others.
//!
//! # Why the constants are not used in `tracing` macros
//!
//! `tracing` accepts only *literal* field names (`flowcraft.craft = %name`), not
//! constants. Call sites therefore spell the dotted name out; these constants
//! exist for everything that is not a macro — redaction rules, the support
//! bundle, the admin surface, and query documentation. `attr::ALL` is asserted
//! to be dotted-lowercase and duplicate-free so a typo in a constant is caught
//! even though a typo at a call site cannot be.

/// Every well-known field, as it is spelled on the wire.
pub mod attr {
    /// The machine-scoped install identifier.
    pub const INSTALL: &str = "flowcraft.install";
    /// How [`INSTALL`] was obtained: `machine`, `user`, or `ephemeral`.
    pub const INSTALL_SCOPE: &str = "flowcraft.install.scope";
    /// Monotonic counter bumped whenever the install identity is reset.
    pub const INSTALL_EPOCH: &str = "flowcraft.install.epoch";
    /// The daemon run that produced the record.
    pub const SESSION: &str = "flowcraft.session";
    /// The application embedding the platform (bundle id or product name).
    pub const APP: &str = "flowcraft.app";
    /// A stable, non-reversible marker for the local user of a shared machine.
    pub const PRINCIPAL: &str = "flowcraft.principal";

    /// Deployment generation (`0` before the first reload).
    pub const GEN: &str = "flowcraft.gen";
    /// The generation that was replaced.
    pub const GEN_PREVIOUS: &str = "flowcraft.gen.previous";
    /// How many resources the generation planned.
    pub const RESOURCES: &str = "flowcraft.resources";
    /// How many crafts the daemon hosts.
    pub const CRAFTS: &str = "flowcraft.crafts";
    /// How many endpoints the catalog serves.
    pub const ENDPOINTS: &str = "flowcraft.endpoints";
    /// How many grants a session holds.
    pub const GRANTS: &str = "flowcraft.grants";
    /// Which supervisor launched a craft: `exec`, `external`, or `systemd`.
    pub const SUPERVISOR: &str = "flowcraft.supervisor";
    /// A directory the daemon reads or watches.
    pub const DIR: &str = "flowcraft.dir";
    /// An operator command issued over the admin socket.
    pub const COMMAND: &str = "flowcraft.command";
    /// A telemetry setting whose value the process was not built with.
    pub const LOG_SETTING: &str = "flowcraft.log.setting";
    /// Hosted craft instance name.
    pub const CRAFT: &str = "flowcraft.craft";
    /// Composition resource name.
    pub const RESOURCE: &str = "flowcraft.resource";
    /// Composition resource kind, e.g. `sandbox.Runner`.
    pub const RESOURCE_KIND: &str = "flowcraft.resource.kind";
    /// Endpoint name from the kernel catalog.
    pub const ENDPOINT: &str = "flowcraft.endpoint";
    /// Capability identifier, e.g. `workspace.Workspace/v1`.
    pub const CAPABILITY: &str = "flowcraft.capability";
    /// Listener socket path.
    pub const SOCKET: &str = "flowcraft.socket";
    /// Child process stream a captured line came from: `stdout` or `stderr`.
    pub const STREAM: &str = "flowcraft.stream";

    /// Tenant boundary, when one is meaningful.
    pub const TENANT: &str = "tenant";
    /// Records dropped by a rate limiter since the previous record.
    pub const SUPPRESSED: &str = "suppressed";

    /// The gRPC method a server-side span belongs to.
    pub const RPC_METHOD: &str = "rpc.method";

    /// The W3C trace id of the context the record was emitted in.
    pub const TRACE_ID: &str = "trace_id";
    /// The span id within [`TRACE_ID`].
    pub const SPAN_ID: &str = "span_id";

    /// The error kind of a failure, e.g. `not_available`.
    pub const ERROR_KIND: &str = "error.kind";
    /// The human-readable failure message.
    pub const ERROR_MESSAGE: &str = "error.message";
}

/// Every constant in [`attr`], for validation and documentation.
pub const ALL: &[&str] = &[
    attr::INSTALL,
    attr::INSTALL_SCOPE,
    attr::INSTALL_EPOCH,
    attr::SESSION,
    attr::APP,
    attr::PRINCIPAL,
    attr::GEN,
    attr::GEN_PREVIOUS,
    attr::RESOURCES,
    attr::CRAFTS,
    attr::ENDPOINTS,
    attr::GRANTS,
    attr::SUPERVISOR,
    attr::DIR,
    attr::COMMAND,
    attr::LOG_SETTING,
    attr::CRAFT,
    attr::RESOURCE,
    attr::RESOURCE_KIND,
    attr::ENDPOINT,
    attr::CAPABILITY,
    attr::SOCKET,
    attr::STREAM,
    attr::TENANT,
    attr::SUPPRESSED,
    attr::ERROR_KIND,
    attr::ERROR_MESSAGE,
    attr::RPC_METHOD,
    attr::TRACE_ID,
    attr::SPAN_ID,
];

/// Keys that must never appear in a record, matched case-insensitively.
///
/// Used by the redaction layer; listed here so the list is reviewable next to
/// the vocabulary it protects.
pub const SECRET_LIKE: &[&str] = &[
    "token",
    "secret",
    "password",
    "passwd",
    "authorization",
    "bearer",
    "cookie",
    "api_key",
    "apikey",
    "private_key",
];

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_are_dotted_lowercase_and_unique() {
        let mut seen = std::collections::BTreeSet::new();
        for name in ALL {
            assert!(!name.is_empty(), "empty field name");
            for ch in name.chars() {
                assert!(
                    ch.is_ascii_lowercase() || ch.is_ascii_digit() || ch == '.' || ch == '_',
                    "{name:?} contains {ch:?}"
                );
            }
            assert!(!name.starts_with('.') && !name.ends_with('.'), "{name:?}");
            assert!(seen.insert(*name), "duplicate field name {name:?}");
        }
    }

    #[test]
    fn error_keys_share_one_namespace() {
        assert!(attr::ERROR_KIND.starts_with("error."));
        assert!(attr::ERROR_MESSAGE.starts_with("error."));
    }
}

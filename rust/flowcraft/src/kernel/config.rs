//! Host-owned configuration.
//!
//! Two rules shape this file:
//!
//! * `mode: strict` (the default) allows only endpoint targets matching
//!   `allow_targets`. A tenant document cannot widen it because the kernel
//!   never reads targets from a deployment document.
//! * TLS material is a **reference**, never inline bytes. A config that carries
//!   a PEM block is rejected at startup, before anything binds.

use std::collections::{BTreeMap, BTreeSet};
use std::path::{Path, PathBuf};

use crate::error::{Error, Result};
use globset::{Glob, GlobSet, GlobSetBuilder};
use serde::{Deserialize, Serialize};

/// How permissive endpoint targets are.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum Mode {
    /// Every target must match `allow_targets`. The only safe mode for
    /// documents a tenant authored.
    #[default]
    Strict,
    /// Any syntactically valid target. Only for a host operator's own document.
    Trusted,
}

/// The whole host configuration.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Config {
    /// Admin surface.
    #[serde(default)]
    pub listen: ListenConfig,
    /// Capability endpoints.
    #[serde(default)]
    pub remote: RemoteConfig,
    /// Grant lifetime.
    #[serde(default)]
    pub grants: GrantsConfig,
    /// Bootstrap secrets (endpoint TLS material), host-owned.
    #[serde(default)]
    pub bootstrap: BootstrapConfig,
    /// Craft instances this daemon hosts, keyed by name.
    #[serde(default)]
    pub crafts: BTreeMap<String, CraftConfig>,
    /// Telemetry defaults. Command-line flags win; see `Platform::logging`.
    #[serde(default)]
    pub log: LogConfig,
}

/// How much the daemon records, and where.
///
/// Host-owned, like the rest of this file, and deliberately plain strings: the
/// kernel does not parse filter syntax or formats, so a bad value is reported by
/// the telemetry pipeline (which can fall back) rather than rejected here, where
/// it could stop a deployment that would otherwise run.
///
/// `level`, `targets` and `redact` take effect on reload. `format`, `file` and
/// `ring_bytes` decide how the process was built at startup, so changing them
/// needs a restart; the daemon says so once instead of silently ignoring them.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LogConfig {
    /// Filter directives, e.g. `info,flowcraft_kernel=debug`.
    #[serde(default)]
    pub level: String,
    /// Per-target overrides, applied after `level`.
    #[serde(default)]
    pub targets: BTreeMap<String, String>,
    /// `text` or `json`. Empty keeps the profile default.
    #[serde(default)]
    pub format: String,
    /// Rotating log file. Empty disables the file sink.
    #[serde(default)]
    pub file: String,
    /// Size that triggers a rotation.
    #[serde(default = "default_log_file_max_bytes")]
    pub file_max_bytes: u64,
    /// How many rotated files to keep. Zero truncates instead.
    #[serde(default = "default_log_file_backups")]
    pub file_backups: u8,
    /// Bytes of recent output kept in memory. Zero disables the ring.
    #[serde(default)]
    pub ring_bytes: usize,
    /// Replace credential-looking field values. Defaults to on.
    #[serde(default)]
    pub redact: Option<bool>,
    /// Ship spans and records to an OTLP collector. Absent means local only,
    /// which is what a client build always is.
    #[serde(default)]
    pub otlp: Option<OtlpConfig>,
    /// Cap repeated warnings and errors to this many per minute. `0` disables
    /// the cap; unset keeps the default.
    #[serde(default)]
    pub throttle_per_minute: Option<u32>,
    /// What to do with a craft's own stdout and stderr.
    ///
    /// `""` decides per supervisor: a process this daemon spawns is captured, a
    /// systemd unit is left to the journal. `capture` turns the child's output
    /// into records attributed to the craft; `inherit` hands it the daemon's own
    /// streams (the old behaviour, and indistinguishable in the log); `ignore`
    /// discards it.
    #[serde(default)]
    pub craft_stdio: String,
}

/// Where to ship telemetry.
///
/// The endpoint is a base URL (`http://collector:4318`); the per-signal path is
/// appended by the exporter. TLS is on unless `insecure`, and the API key of a
/// managed backend goes in `headers`, never in the URL.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OtlpConfig {
    /// Base endpoint, e.g. `http://otel-collector.monitoring:4318`.
    pub endpoint: String,
    /// Headers on every export request.
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
    /// Skip TLS. Only for a collector on the same host or pod network.
    #[serde(default)]
    pub insecure: bool,
}

/// How a craft's own output is treated.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CraftStdio {
    /// Turn each line into a record attributed to the craft.
    Capture,
    /// Hand the child the daemon's streams.
    Inherit,
    /// Discard it.
    Ignore,
}

impl CraftStdio {
    /// Parse a configured value, falling back to `default` when unset.
    ///
    /// An unknown value is a configuration error; returning `None` lets the
    /// caller report it with the field path it came from.
    #[must_use]
    pub fn parse(value: &str, default: Self) -> Option<Self> {
        match value.trim() {
            "" => Some(default),
            "capture" => Some(Self::Capture),
            "inherit" => Some(Self::Inherit),
            "ignore" => Some(Self::Ignore),
            _ => None,
        }
    }
}

fn default_log_file_max_bytes() -> u64 {
    8 * 1024 * 1024
}

fn default_log_file_backups() -> u8 {
    3
}

impl LogConfig {
    /// The filter directives, `level` plus `targets`, in `EnvFilter` syntax.
    ///
    /// Empty when the section says nothing, so the caller keeps its default.
    #[must_use]
    pub fn directives(&self) -> String {
        let mut parts: Vec<String> = Vec::with_capacity(self.targets.len() + 1);
        if !self.level.trim().is_empty() {
            parts.push(self.level.trim().to_owned());
        }
        for (target, level) in &self.targets {
            if !target.trim().is_empty() && !level.trim().is_empty() {
                parts.push(format!("{}={}", target.trim(), level.trim()));
            }
        }
        parts.join(",")
    }

    /// Whether this section asks for anything at all.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        *self == Self::default()
    }

    fn validate(&self) -> Result<()> {
        if !self.format.is_empty() && !matches!(self.format.as_str(), "text" | "json") {
            return Err(Error::validation(format!(
                "log.format must be \"text\" or \"json\", not {:?}",
                self.format
            )));
        }
        if CraftStdio::parse(&self.craft_stdio, CraftStdio::Inherit).is_none() {
            return Err(Error::validation(format!(
                "log.craft_stdio must be \"capture\", \"inherit\" or \"ignore\", not {:?}",
                self.craft_stdio
            )));
        }
        if self
            .throttle_per_minute
            .is_some_and(|limit| limit > 100_000)
        {
            return Err(Error::validation(
                "log.throttle_per_minute must be at most 100000",
            ));
        }
        if self.file_backups > 20 {
            return Err(Error::validation("log.file_backups must be at most 20"));
        }
        if !self.file.is_empty() && self.file_max_bytes < 1024 {
            return Err(Error::validation(
                "log.file_max_bytes must be at least 1024 (or 0 to disable rotation)",
            ));
        }
        Ok(())
    }
}

/// One craft instance to launch.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CraftConfig {
    /// `argv[0]` plus arguments.
    pub argv: Vec<String>,
    /// Socket the craft serves on.
    pub socket: String,
    /// Tenant the session belongs to.
    #[serde(default)]
    pub tenant: String,
    /// Capabilities to grant, each pointing at a catalog endpoint.
    #[serde(default)]
    pub grants: Vec<CraftGrant>,
    /// Extra environment for the child.
    #[serde(default)]
    pub env: BTreeMap<String, String>,
    /// How long to wait for the craft's socket.
    #[serde(default = "default_ready_timeout_secs")]
    pub ready_timeout_secs: u64,
    /// Who launches the craft: `exec` (this daemon), `external` (already
    /// running), or `systemd` (a unit this daemon starts and stops).
    #[serde(default = "default_supervisor")]
    pub supervisor: String,
    /// systemd unit name, when `supervisor: systemd`. Empty derives
    /// `flowcraft-<name>.service`.
    #[serde(default)]
    pub unit: String,
    /// `systemctl` binary, when `supervisor: systemd`. Empty uses `PATH`.
    #[serde(default)]
    pub systemctl: String,
}

fn default_supervisor() -> String {
    "exec".to_owned()
}

/// One capability grant handed to a craft.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CraftGrant {
    /// Capability, e.g. `workspace.Workspace/v1`.
    pub capability: String,
    /// Catalog endpoint that serves it.
    pub endpoint: String,
    /// Capability-specific limits, frozen into the grant.
    #[serde(default)]
    pub scope: serde_json::Value,
}

fn default_ready_timeout_secs() -> u64 {
    10
}

/// Where the kernel reads bootstrap secrets from, in order.
///
/// These are not deployment secrets: they must be available before any
/// resource is built, so they come from the host (environment or a directory of
/// files).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BootstrapConfig {
    /// Environment variable prefix; empty disables the source.
    #[serde(default)]
    pub env_prefix: String,
    /// Directory whose files are named by `${secret:NAME}`; empty disables it.
    #[serde(default)]
    pub dir: String,
    /// Reload when files under `dir` change. Defaults to on when `dir` is set.
    #[serde(default)]
    pub watch: Option<bool>,
}

/// The admin socket.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ListenConfig {
    /// `admin.sock` location. Unix only in this cut.
    #[serde(default = "default_admin")]
    pub admin: String,
}

impl Default for ListenConfig {
    fn default() -> Self {
        Self {
            admin: default_admin(),
        }
    }
}

fn default_admin() -> String {
    "unix:///run/flowcraft/admin.sock".to_owned()
}

/// Endpoint catalog and target policy.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RemoteConfig {
    /// Target policy.
    #[serde(default)]
    pub mode: Mode,
    /// Glob patterns a target must match in strict mode.
    #[serde(default)]
    pub allow_targets: Vec<String>,
    /// The logical endpoints, keyed by name.
    #[serde(default)]
    pub endpoints: BTreeMap<String, EndpointConfig>,
}

/// One endpoint.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EndpointConfig {
    /// Transport address, e.g. `unix:///run/flowcraft/ws.sock`.
    pub target: String,
    /// The tenant this endpoint serves. Empty means shared.
    #[serde(default)]
    pub tenant: String,
    /// Capabilities served. Empty means "everything asked of it".
    #[serde(default)]
    pub capabilities: Vec<String>,
    /// TLS material, as secret references.
    #[serde(default)]
    pub tls: Option<TlsConfig>,
}

/// TLS material. Every field is a `${secret:...}` reference or empty.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TlsConfig {
    /// Server certificate chain.
    #[serde(default)]
    pub cert: String,
    /// Server private key.
    #[serde(default)]
    pub key: String,
    /// Client CA bundle, for mutual TLS.
    #[serde(default)]
    pub client_ca: String,
    /// CA bundle for a client endpoint.
    #[serde(default)]
    pub ca: String,
    /// Client certificate.
    #[serde(default)]
    pub client_cert: String,
    /// Client private key.
    #[serde(default)]
    pub client_key: String,
    /// Expected server name.
    #[serde(default)]
    pub server_name: String,
}

/// Grant lifetime.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GrantsConfig {
    /// Default TTL in seconds.
    #[serde(default = "default_ttl_secs")]
    pub ttl_secs: u64,
}

impl Default for GrantsConfig {
    fn default() -> Self {
        Self {
            ttl_secs: default_ttl_secs(),
        }
    }
}

fn default_ttl_secs() -> u64 {
    900
}

/// A parsed endpoint target.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Target {
    /// A unix-domain socket path.
    Unix(PathBuf),
    /// A DNS address; parsed but not served by this cut.
    Dns(String),
}

impl Target {
    /// The target as the document spells it.
    #[must_use]
    pub fn as_uri(&self) -> String {
        match self {
            Self::Unix(path) => format!("unix://{}", path.display()),
            Self::Dns(address) => format!("dns:///{address}"),
        }
    }

    /// The scheme spelling, for diagnostics.
    #[must_use]
    pub const fn scheme(&self) -> &'static str {
        match self {
            Self::Unix(_) => "unix",
            Self::Dns(_) => "dns",
        }
    }
}

/// Parse `unix:///abs/path` or `dns:///host:port`.
pub fn parse_target(raw: &str) -> Result<Target> {
    let raw = raw.trim();
    if let Some(path) = raw.strip_prefix("unix://") {
        if path.is_empty() {
            return Err(Error::validation(format!(
                "target {raw:?}: unix target needs a path"
            )));
        }
        let path = Path::new(path);
        if !path.is_absolute() {
            return Err(Error::validation(format!(
                "target {raw:?}: unix path must be absolute"
            )));
        }
        return Ok(Target::Unix(path.to_path_buf()));
    }
    if let Some(authority) = raw.strip_prefix("dns://") {
        let authority = authority.trim_start_matches('/');
        if authority.is_empty() {
            return Err(Error::validation(format!(
                "target {raw:?}: dns target needs host:port"
            )));
        }
        if !authority.contains(':') {
            return Err(Error::validation(format!(
                "target {raw:?}: dns target needs an explicit port"
            )));
        }
        return Ok(Target::Dns(authority.to_owned()));
    }
    Err(Error::validation(format!(
        "target {raw:?}: must start with unix:// or dns://"
    )))
}

impl Config {
    /// Parse and validate a configuration document (JSON or YAML).
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        let text = std::str::from_utf8(bytes)
            .map_err(|error| Error::validation(format!("config is not UTF-8: {error}")))?;
        let value = crate::resource::decode::document_value(text)?;
        let config: Self = crate::resource::settings(&value)?;
        config.validate()?;
        Ok(config)
    }

    /// Validate without I/O.
    pub fn validate(&self) -> Result<()> {
        if self.grants.ttl_secs == 0 {
            return Err(Error::validation("grants.ttl_secs must be positive"));
        }
        self.log.validate()?;
        parse_target(&self.listen.admin)?;

        let allow = build_matcher(&self.remote.allow_targets)?;
        let mut targets = BTreeSet::new();
        for (name, endpoint) in &self.remote.endpoints {
            crate::resource::validate_name("endpoint", name)?;
            parse_target(&endpoint.target)?;
            if !targets.insert(endpoint.target.clone()) {
                return Err(Error::conflict(format!(
                    "endpoints: target {:?} is declared twice",
                    endpoint.target
                )));
            }
            if self.remote.mode == Mode::Strict && !allow.is_match(&endpoint.target) {
                return Err(Error::forbidden(format!(
                    "endpoints.{name}: target {:?} matches no allow_targets entry",
                    endpoint.target
                )));
            }
            for capability in &endpoint.capabilities {
                if capability.trim().is_empty() {
                    return Err(Error::validation(format!(
                        "endpoints.{name}: capability names must not be empty"
                    )));
                }
            }
            if let Some(tls) = &endpoint.tls {
                tls.validate(name)?;
            }
        }

        for (name, craft) in &self.crafts {
            crate::resource::validate_name("craft", name)?;
            // `external` and `systemd` crafts are launched by someone else;
            // argv is only used by the `exec` supervisor.
            if !matches!(craft.supervisor.as_str(), "external" | "systemd")
                && (craft.argv.is_empty() || craft.argv[0].trim().is_empty())
            {
                return Err(Error::validation(format!("crafts.{name}: argv is empty")));
            }
            if craft.socket.trim().is_empty() || !Path::new(&craft.socket).is_absolute() {
                return Err(Error::validation(format!(
                    "crafts.{name}: socket must be an absolute path"
                )));
            }
            if !matches!(craft.supervisor.as_str(), "exec" | "external" | "systemd") {
                return Err(Error::validation(format!(
                    "crafts.{name}: supervisor {:?} must be \"exec\", \"external\" or \"systemd\"",
                    craft.supervisor
                )));
            }
            if !craft.unit.trim().is_empty()
                && (craft.unit.contains(['/', ' ', '\t']) || craft.unit != craft.unit.trim())
            {
                return Err(Error::validation(format!(
                    "crafts.{name}: unit {:?} must be a unit name, not a path",
                    craft.unit
                )));
            }
            if craft.ready_timeout_secs == 0 {
                return Err(Error::validation(format!(
                    "crafts.{name}: ready_timeout_secs must be positive"
                )));
            }
            for grant in &craft.grants {
                if grant.capability.trim().is_empty() || grant.endpoint.trim().is_empty() {
                    return Err(Error::validation(format!(
                        "crafts.{name}.grants: capability and endpoint are required"
                    )));
                }
                let Some(endpoint) = self.remote.endpoints.get(&grant.endpoint) else {
                    return Err(Error::not_found(format!(
                        "crafts.{name}.grants: endpoint {:?} is not in the catalog",
                        grant.endpoint
                    )));
                };
                if !endpoint.capabilities.is_empty()
                    && !endpoint
                        .capabilities
                        .iter()
                        .any(|item| item == &grant.capability)
                {
                    return Err(Error::not_available(format!(
                        "crafts.{name}.grants: endpoint {:?} does not serve {:?}",
                        grant.endpoint, grant.capability
                    )));
                }
                if !endpoint.tenant.is_empty() && endpoint.tenant != craft.tenant {
                    return Err(Error::forbidden(format!(
                        "crafts.{name}.grants: endpoint {:?} serves tenant {:?}, not {:?}",
                        grant.endpoint, endpoint.tenant, craft.tenant
                    )));
                }
            }
        }

        // A DNS target with no TLS is a plaintext remote capability.
        for (name, endpoint) in &self.remote.endpoints {
            if matches!(parse_target(&endpoint.target)?, Target::Dns(_)) && endpoint.tls.is_none() {
                return Err(Error::validation(format!(
                    "endpoints.{name}: a dns target requires tls"
                )));
            }
        }
        Ok(())
    }
}

impl TlsConfig {
    /// TLS material must be a secret reference; an inline PEM is rejected.
    pub fn validate(&self, endpoint: &str) -> Result<()> {
        for (field, value) in [
            ("cert", &self.cert),
            ("key", &self.key),
            ("client_ca", &self.client_ca),
            ("ca", &self.ca),
            ("client_cert", &self.client_cert),
            ("client_key", &self.client_key),
        ] {
            if value.is_empty() {
                continue;
            }
            if !value.starts_with("${secret:") || !value.ends_with('}') {
                return Err(Error::validation(format!(
                    "endpoints.{endpoint}.tls.{field}: must be a ${{secret:...}} reference, not inline material"
                )));
            }
        }
        Ok(())
    }
}

fn build_matcher(patterns: &[String]) -> Result<GlobSet> {
    let mut builder = GlobSetBuilder::new();
    for pattern in patterns {
        let glob = Glob::new(pattern).map_err(|error| {
            Error::validation(format!("allow_targets: {pattern:?} is not a glob: {error}"))
        })?;
        builder.add(glob);
    }
    builder
        .build()
        .map_err(|error| Error::validation(format!("allow_targets: {error}")))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn endpoint(target: &str) -> EndpointConfig {
        EndpointConfig {
            target: target.to_owned(),
            tenant: String::new(),
            capabilities: Vec::new(),
            tls: None,
        }
    }

    #[test]
    fn strict_mode_requires_a_matching_target() {
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Strict,
                allow_targets: vec!["unix:///run/flowcraft/*".to_owned()],
                endpoints: BTreeMap::from([
                    ("ok".to_owned(), endpoint("unix:///run/flowcraft/ws.sock")),
                    ("escape".to_owned(), endpoint("unix:///tmp/ws.sock")),
                ]),
            },
            ..Config::default()
        };
        let error = config.validate().unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Forbidden);
        assert!(error.to_string().contains("escape"), "{error}");
    }

    #[test]
    fn strict_mode_with_no_allow_targets_allows_nothing() {
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Strict,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([(
                    "ws".to_owned(),
                    endpoint("unix:///run/flowcraft/ws.sock"),
                )]),
            },
            ..Config::default()
        };
        assert!(config.validate().is_err());
    }

    #[test]
    fn trusted_mode_skips_the_target_policy() {
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([("ws".to_owned(), endpoint("unix:///tmp/ws.sock"))]),
            },
            ..Config::default()
        };
        config.validate().unwrap();
    }

    #[test]
    fn duplicate_targets_are_a_conflict() {
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([
                    ("a".to_owned(), endpoint("unix:///tmp/a.sock")),
                    ("b".to_owned(), endpoint("unix:///tmp/a.sock")),
                ]),
            },
            ..Config::default()
        };
        assert_eq!(
            config.validate().unwrap_err().kind(),
            crate::error::ErrorKind::Conflict
        );
    }

    #[test]
    fn inline_pem_is_rejected() {
        let mut endpoint = endpoint("unix:///tmp/a.sock");
        endpoint.tls = Some(TlsConfig {
            cert: "-----BEGIN CERTIFICATE-----".to_owned(),
            ..TlsConfig::default()
        });
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([("a".to_owned(), endpoint)]),
            },
            ..Config::default()
        };
        let error = config.validate().unwrap_err();
        assert!(error.to_string().contains("secret"), "{error}");
    }

    #[test]
    fn dns_requires_tls() {
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([(
                    "farm".to_owned(),
                    endpoint("dns:///sandbox.internal:443"),
                )]),
            },
            ..Config::default()
        };
        let error = config.validate().unwrap_err();
        assert!(error.to_string().contains("requires tls"), "{error}");
    }

    #[test]
    fn targets_are_parsed_strictly() {
        assert_eq!(
            parse_target("unix:///run/x.sock").unwrap(),
            Target::Unix(PathBuf::from("/run/x.sock"))
        );
        assert!(parse_target("unix://relative.sock").is_err());
        assert!(parse_target("dns:///host").is_err());
        assert!(parse_target("http://example.com").is_err());
    }

    #[test]
    fn a_craft_grant_must_name_a_catalog_endpoint() {
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([(
                    "ws".to_owned(),
                    EndpointConfig {
                        target: "unix:///tmp/ws.sock".to_owned(),
                        tenant: String::new(),
                        capabilities: vec!["workspace.Workspace/v1".to_owned()],
                        tls: None,
                    },
                )]),
            },
            crafts: BTreeMap::from([(
                "demo".to_owned(),
                CraftConfig {
                    argv: vec!["/bin/craft".to_owned()],
                    socket: "/tmp/demo.sock".to_owned(),
                    tenant: String::new(),
                    grants: vec![CraftGrant {
                        capability: "workspace.Workspace/v1".to_owned(),
                        endpoint: "nope".to_owned(),
                        scope: serde_json::json!({}),
                    }],
                    env: BTreeMap::new(),
                    ready_timeout_secs: 10,
                    supervisor: "exec".to_owned(),
                    unit: String::new(),
                    systemctl: String::new(),
                },
            )]),
            ..Config::default()
        };
        let error = config.validate().unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::NotFound);
        assert!(error.to_string().contains("crafts.demo"), "{error}");
    }

    #[test]
    fn a_craft_grant_capability_must_be_served() {
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([(
                    "ws".to_owned(),
                    EndpointConfig {
                        target: "unix:///tmp/ws.sock".to_owned(),
                        tenant: String::new(),
                        capabilities: vec!["workspace.Workspace/v1".to_owned()],
                        tls: None,
                    },
                )]),
            },
            crafts: BTreeMap::from([(
                "demo".to_owned(),
                CraftConfig {
                    argv: vec!["/bin/craft".to_owned()],
                    socket: "/tmp/demo.sock".to_owned(),
                    tenant: String::new(),
                    grants: vec![CraftGrant {
                        capability: "sandbox.Runner/v1".to_owned(),
                        endpoint: "ws".to_owned(),
                        scope: serde_json::json!({}),
                    }],
                    env: BTreeMap::new(),
                    ready_timeout_secs: 10,
                    supervisor: "exec".to_owned(),
                    unit: String::new(),
                    systemctl: String::new(),
                },
            )]),
            ..Config::default()
        };
        let error = config.validate().unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
    }

    #[test]
    fn a_craft_socket_must_be_absolute() {
        let config = Config {
            crafts: BTreeMap::from([(
                "demo".to_owned(),
                CraftConfig {
                    argv: vec!["/bin/craft".to_owned()],
                    socket: "relative.sock".to_owned(),
                    tenant: String::new(),
                    grants: Vec::new(),
                    env: BTreeMap::new(),
                    ready_timeout_secs: 10,
                    supervisor: "exec".to_owned(),
                    unit: String::new(),
                    systemctl: String::new(),
                },
            )]),
            ..Config::default()
        };
        assert_eq!(
            config.validate().unwrap_err().kind(),
            crate::error::ErrorKind::Validation
        );
    }

    #[test]
    fn yaml_and_json_configs_parse_alike() {
        let yaml = br#"
remote:
  mode: strict
  allow_targets: ["unix:///run/flowcraft/*"]
  endpoints:
    ws: { target: "unix:///run/flowcraft/ws.sock" }
"#;
        let config = Config::parse(yaml).unwrap();
        assert_eq!(config.remote.mode, Mode::Strict);
        assert_eq!(config.remote.endpoints.len(), 1);
        assert_eq!(config.grants.ttl_secs, 900);
    }
}

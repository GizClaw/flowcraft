//! Choosing a profile, and installing the pipeline for one command.
//!
//! This is the only place that decides *what shape* the daemon's telemetry
//! takes: which profile, which format, which filter, which sinks. Keeping the
//! decision here rather than in `flowcraft_telemetry` means the library stays a
//! mechanism while the binary stays the policy.
//!
//! # Two phases, because the pipeline outlives the configuration
//!
//! The pipeline is installed *before* `platform.yaml` is read — a configuration
//! error has to be loggable — but the level lives in that file. So the first
//! install uses flags, the environment and the profile defaults, and
//! [`apply_config`] afterwards replaces the filter in place.
//!
//! Only the filter can change that way. The format and the sinks are chosen when
//! the subscriber is built, so a configuration that asks for a different one gets
//! one warning and a restart, never a silent no-op.

use std::io::IsTerminal;
use std::sync::Arc;

use flowcraft::kernel::config::LogConfig;
use flowcraft::kernel::identity::{MachineIdentity, Scope};
use flowcraft::logerr;
use flowcraft::telemetry::sinks::file as file_sink;
use flowcraft::telemetry::{Format, Guard, Install, Profile, Resource, install, session_id};

use crate::cli::{Command, LogSettings};

/// The service name every record of this binary carries.
const SERVICE: &str = "flowcraftd";

/// A client daemon keeps recent output in memory for its support bundle.
const CLIENT_RING_BYTES: usize = flowcraft::telemetry::sinks::ring::DEFAULT_BYTES;

/// What was actually installed.
///
/// Kept next to the guard so [`apply_config`] can tell "already like that" from
/// "needs a restart", and so the admin surface can report the real state.
#[derive(Debug)]
pub struct Applied {
    /// The installed pipeline.
    pub guard: Arc<Guard>,
    /// The request it was built from.
    pub install: Install,
    /// Whether the filter came from a flag or the environment, in which case a
    /// configuration file must not override it.
    pub directives_from_host: bool,
}

impl Applied {
    /// A stand-in for a process whose pipeline could not be installed.
    ///
    /// Still describes the binary, so an admin `diag` reports the same service
    /// name a working pipeline would.
    #[must_use]
    pub fn disabled() -> Self {
        Self {
            guard: Arc::new(Guard::disabled()),
            install: Install::new(Profile::Service).with_resource(Resource {
                service: SERVICE.to_owned(),
                version: env!("CARGO_PKG_VERSION").to_owned(),
                ..Resource::default()
            }),
            directives_from_host: true,
        }
    }
}

/// Install the pipeline for `command`.
///
/// The profile follows the command: a daemon is a service, a one-shot operator
/// command is not. `--profile client` opts into the client-daemon defaults.
pub fn install_for(
    command: &Command,
    settings: &LogSettings,
    identity: &MachineIdentity,
    config: Option<&LogConfig>,
) -> flowcraft::error::Result<Applied> {
    let profile = profile_for(command, settings);
    let directives_from_host = settings.provides_directives();
    let config = config.filter(|config| !config.is_empty());
    let request = Install::new(profile)
        .with_format(format_for(command, settings, config))
        .with_ansi(profile != Profile::Cli && std::io::stderr().is_terminal())
        .with_directives(directives_for(profile, settings, config))
        .with_resource(resource_from(identity))
        .with_ring(ring_for(profile, settings, config))
        .with_redaction(config.and_then(|config| config.redact).unwrap_or(true))
        .with_throttle(throttle_for(config));
    let request = match otlp_for(config) {
        Some(otlp) => request.with_otlp(otlp),
        None => request,
    };
    // A path from the configuration brings that section's bounds with it; a
    // path from a flag, with no section behind it, uses the defaults.
    let configured_path = config
        .map(|config| config.file.trim())
        .filter(|path| !path.is_empty());
    let file = settings
        .file
        .as_deref()
        .map(str::trim)
        .filter(|path| !path.is_empty())
        .map(|path| match config {
            Some(config) if !config.file.trim().is_empty() => file_sink::Options {
                path: path.into(),
                max_bytes: config.file_max_bytes,
                backups: config.file_backups,
            },
            _ => file_sink::Options::new(path),
        })
        .or_else(|| {
            configured_path.map(|path| file_sink::Options {
                path: path.into(),
                max_bytes: config.map(|config| config.file_max_bytes).unwrap_or(0),
                backups: config.map(|config| config.file_backups).unwrap_or(0),
            })
        });
    let request = match file {
        Some(options) => request.with_file(options),
        None => request,
    };
    // A bad filter is a configuration typo, not a reason to run blind: fall
    // back to the profile default and say so through the pipeline that now
    // exists.
    let (guard, fell_back) = match install(request.clone()) {
        Ok(guard) => (guard, false),
        Err(error) if error.kind() == flowcraft::error::ErrorKind::Validation => {
            let fallback = request
                .clone()
                .with_directives(profile.default_directives());
            let guard = install(fallback)?;
            logerr!(
                warn,
                error,
                "log.level is invalid; using the profile default"
            );
            (guard, true)
        }
        Err(error) => return Err(error),
    };
    let install = if fell_back {
        request.with_directives(profile.default_directives().to_owned())
    } else {
        request
    };
    Ok(Applied {
        guard: Arc::new(guard),
        install,
        directives_from_host,
    })
}

/// Apply the `log:` section to a pipeline that is already running.
///
/// Returns the problems that need a restart rather than dropping them: a host
/// that cannot apply a setting must still tell its operator which one.
pub fn apply_config(applied: &Applied, config: &LogConfig) {
    if config.is_empty() {
        return;
    }
    if !applied.directives_from_host {
        let directives = config.directives();
        if !directives.is_empty()
            && let Err(error) = applied.guard.set_directives(&directives)
        {
            // A bad filter must not cost a healthy deployment: keep the one in
            // force and say why.
            logerr!(
                warn,
                error,
                "log.level is invalid; keeping the running filter"
            );
        }
    }
    for ignored in restart_reasons(applied, config) {
        tracing::warn!(
            flowcraft.log.setting = ignored.as_str(),
            "log setting needs a restart to take effect"
        );
    }
}

/// The configured settings this process was not built with.
fn restart_reasons(applied: &Applied, config: &LogConfig) -> Vec<String> {
    let mut reasons = Vec::new();
    if let Some(requested) = Format::parse(&config.format)
        && requested != applied.install.format
    {
        reasons.push(format!("log.format={}", config.format));
    }
    let configured_file = config.file.trim();
    let installed_file = applied
        .install
        .file
        .as_ref()
        .map(|options| options.path.display().to_string())
        .unwrap_or_default();
    if configured_file != installed_file {
        reasons.push(format!("log.file={configured_file}"));
    }
    if config.ring_bytes > 0 && config.ring_bytes != applied.install.ring_bytes {
        reasons.push(format!("log.ring_bytes={}", config.ring_bytes));
    }
    reasons
}

/// The profile a command runs under.
fn profile_for(command: &Command, settings: &LogSettings) -> Profile {
    match settings.profile.as_deref() {
        Some("client") | Some("embedded") => return Profile::Embedded,
        Some("service") => return Profile::Service,
        Some("cli") => return Profile::Cli,
        _ => {}
    }
    match command {
        Command::Serve { .. } | Command::CraftStub { .. } => Profile::Service,
        Command::Help
        | Command::Version
        | Command::Catalog { .. }
        | Command::Check { .. }
        | Command::Ctl { .. }
        | Command::Diag { .. } => Profile::Cli,
    }
}

/// `--log-format`, else the profile default.
///
/// A service answers to machines by default, but a developer running it in a
/// terminal wants to read it, so an interactive stderr wins.
fn format_for(command: &Command, settings: &LogSettings, config: Option<&LogConfig>) -> Format {
    if let Some(format) = settings.format.as_deref().and_then(Format::parse) {
        return format;
    }
    if let Some(format) = config
        .map(|config| config.format.as_str())
        .and_then(Format::parse)
    {
        return format;
    }
    match command {
        // The stub is a development aid; nobody scrapes its output.
        Command::CraftStub { .. } => Format::Text,
        _ if profile_for(command, settings) == Profile::Cli => Format::Text,
        _ if profile_for(command, settings) == Profile::Embedded => Format::Text,
        _ if std::io::stderr().is_terminal() => Format::Text,
        _ => Format::Json,
    }
}

/// `RUST_LOG`, else `--log-level`, else `-v`/`-q`, else the profile default.
fn directives_for(profile: Profile, settings: &LogSettings, config: Option<&LogConfig>) -> String {
    if let Some(value) = std::env::var("RUST_LOG")
        .ok()
        .filter(|value| !value.is_empty())
    {
        return value;
    }
    if let Some(level) = settings.level.as_deref() {
        return level.to_owned();
    }
    if settings.verbose {
        return "debug".to_owned();
    }
    if settings.quiet {
        return "warn".to_owned();
    }
    if let Some(directives) = config.map(LogConfig::directives)
        && !directives.is_empty()
    {
        return directives;
    }
    profile.default_directives().to_owned()
}

/// A client daemon keeps a ring by default; a server does not need one.
fn ring_for(profile: Profile, settings: &LogSettings, config: Option<&LogConfig>) -> usize {
    if let Some(bytes) = config.map(|config| config.ring_bytes)
        && bytes > 0
    {
        return bytes;
    }
    if settings.ring || profile == Profile::Embedded {
        CLIENT_RING_BYTES
    } else {
        0
    }
}

/// The collector, when one is configured.
fn otlp_for(config: Option<&LogConfig>) -> Option<flowcraft::telemetry::Otlp> {
    config
        .and_then(|config| config.otlp.as_ref())
        .filter(|otlp| !otlp.endpoint.trim().is_empty())
        .map(|otlp| flowcraft::telemetry::Otlp {
            endpoint: otlp.endpoint.trim().to_owned(),
            headers: otlp
                .headers
                .iter()
                .map(|(key, value)| (key.clone(), value.clone()))
                .collect(),
            insecure: otlp.insecure,
        })
}

/// A cap on repeated failures: on by default, because a device that fills its
/// disk is a worse failure than a log that repeats itself.
fn throttle_for(config: Option<&LogConfig>) -> flowcraft::telemetry::throttle::Policy {
    use std::time::Duration;

    use flowcraft::telemetry::throttle::Policy;

    match config.and_then(|config| config.throttle_per_minute) {
        Some(0) => Policy::disabled(),
        Some(per_window) => Policy {
            per_window,
            window: Duration::from_secs(60),
        },
        None => Policy::default(),
    }
}

fn resource_from(identity: &MachineIdentity) -> Resource {
    Resource {
        service: SERVICE.to_owned(),
        version: env!("CARGO_PKG_VERSION").to_owned(),
        app: None,
        install: Some(identity.install().to_owned()),
        install_scope: Some(identity.scope().as_str().to_owned()),
        install_epoch: identity.epoch(),
        session: session_id(),
        principal: identity.principal(),
    }
}

/// Warn once when the identity is degraded.
///
/// An ephemeral identity still produces records; they just cannot be tied to a
/// machine, which is exactly what support needs, so it is worth one line.
pub fn report_identity(identity: &MachineIdentity, error: Option<flowcraft::error::Error>) {
    let Some(error) = error else {
        return;
    };
    let ephemeral = identity.scope() == Scope::Ephemeral;
    if ephemeral {
        logerr!(
            warn,
            error,
            "machine identity is ephemeral; records cannot be correlated across runs",
            flowcraft.install = identity.install(),
            flowcraft.install.scope = identity.scope().as_str()
        );
    } else if error.kind() != flowcraft::error::ErrorKind::NotFound {
        logerr!(
            warn,
            error,
            "machine identity is degraded",
            flowcraft.install = identity.install(),
            flowcraft.install.scope = identity.scope().as_str()
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn serve() -> Command {
        Command::Serve {
            config: "platform.yaml".to_owned(),
            deploys: vec!["deploy.yaml".to_owned()],
        }
    }

    fn check() -> Command {
        Command::Check {
            config: None,
            deploys: vec!["deploy.yaml".to_owned()],
            json: false,
        }
    }

    #[test]
    fn a_daemon_is_a_service_and_a_command_is_not() {
        let settings = LogSettings::default();
        assert_eq!(profile_for(&serve(), &settings), Profile::Service);
        assert_eq!(profile_for(&check(), &settings), Profile::Cli);
    }

    #[test]
    fn a_client_daemon_keeps_a_ring_and_reads_as_text() {
        let settings = LogSettings {
            profile: Some("client".to_owned()),
            ..LogSettings::default()
        };
        let profile = profile_for(&serve(), &settings);
        assert_eq!(profile, Profile::Embedded);
        assert_eq!(ring_for(profile, &settings, None), CLIENT_RING_BYTES);
        assert_eq!(format_for(&serve(), &settings, None), Format::Text);
    }

    #[test]
    fn a_server_does_not_keep_a_ring_unless_asked() {
        let settings = LogSettings::default();
        assert_eq!(ring_for(Profile::Service, &settings, None), 0);
        let asked = LogSettings {
            ring: true,
            ..LogSettings::default()
        };
        assert_eq!(ring_for(Profile::Service, &asked, None), CLIENT_RING_BYTES);
    }

    #[test]
    fn commands_do_not_emit_timestamps_or_targets() {
        let install = Install::new(Profile::Cli);
        assert!(!install.timestamps);
        assert!(!install.targets);
        assert_eq!(install.directives, "warn");
    }

    #[test]
    fn a_configured_throttle_replaces_the_default() {
        let config = LogConfig {
            throttle_per_minute: Some(5),
            ..LogConfig::default()
        };
        assert_eq!(throttle_for(Some(&config)).per_window, 5);
        assert!(throttle_for(None).is_enabled(), "on by default");
        let off = LogConfig {
            throttle_per_minute: Some(0),
            ..LogConfig::default()
        };
        assert!(!throttle_for(Some(&off)).is_enabled());
    }

    #[test]
    fn quiet_and_verbose_are_shorthands() {
        let verbose = LogSettings {
            verbose: true,
            ..LogSettings::default()
        };
        assert_eq!(directives_for(Profile::Service, &verbose, None), "debug");
        let quiet = LogSettings {
            quiet: true,
            ..LogSettings::default()
        };
        assert_eq!(directives_for(Profile::Service, &quiet, None), "warn");
    }

    #[test]
    fn an_explicit_level_beats_the_shorthand() {
        let settings = LogSettings {
            level: Some("flowcraft_kernel=trace".to_owned()),
            verbose: true,
            ..LogSettings::default()
        };
        assert_eq!(
            directives_for(Profile::Service, &settings, None),
            "flowcraft_kernel=trace"
        );
        assert!(settings.provides_directives());
    }

    #[test]
    fn a_format_name_is_honoured() {
        let settings = LogSettings {
            format: Some("json".to_owned()),
            ..LogSettings::default()
        };
        assert_eq!(format_for(&serve(), &settings, None), Format::Json);
        assert_eq!(format_for(&check(), &settings, None), Format::Json);
    }

    fn applied() -> Applied {
        Applied {
            guard: Arc::new(Guard::disabled()),
            install: Install::new(Profile::Service).with_ring(0),
            directives_from_host: false,
        }
    }

    #[test]
    fn format_and_sinks_are_reported_as_needing_a_restart() {
        let config = LogConfig {
            format: "json".to_owned(),
            file: "/var/log/flowcraftd.log".to_owned(),
            ring_bytes: 4096,
            ..LogConfig::default()
        };
        let reasons = restart_reasons(&applied(), &config);
        assert!(
            reasons
                .iter()
                .any(|reason| reason.starts_with("log.format"))
        );
        assert!(reasons.iter().any(|reason| reason.starts_with("log.file")));
        assert!(
            reasons
                .iter()
                .any(|reason| reason.starts_with("log.ring_bytes"))
        );
    }

    #[test]
    fn an_unchanged_section_needs_no_restart() {
        let install = Install::new(Profile::Service).with_ring(4096);
        let applied = Applied {
            guard: Arc::new(Guard::disabled()),
            install,
            directives_from_host: false,
        };
        let config = LogConfig {
            ring_bytes: 4096,
            ..LogConfig::default()
        };
        assert!(restart_reasons(&applied, &config).is_empty());
    }
}

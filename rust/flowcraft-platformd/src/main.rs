//! `flowcraftd` — the platform daemon.

use std::process::ExitCode;

use flowcraft::error::Error;
use flowcraft::kernel::identity::MachineIdentity;
use flowcraft_platformd::{assembly, cli, commands, logging};

fn main() -> ExitCode {
    // The daemon is also the in-namespace bridge: `bwrap` re-executes this
    // binary with a reserved argument so that something inside the network
    // namespace can own the proxy port. That path has to be taken before
    // anything else looks at argv, and it exits the process when it applies.
    let _ = flowcraft::caps::sandbox::net::bridge::run_if_invoked();

    // Before anything binds a TLS listener or opens a TLS channel: the
    // dependency graph unifies both rustls providers, and rustls will not pick
    // one by itself (see `kernel::tls::install_provider`).
    flowcraft::kernel::tls::install_provider();

    let args: Vec<String> = std::env::args().skip(1).collect();
    match run(args) {
        Ok(code) => code,
        Err(failure) => {
            eprintln!("flowcraftd: {}", failure.error);
            ExitCode::from(failure.code)
        }
    }
}

/// Whether this command drives a runtime.
fn drives_a_runtime(command: &cli::Command) -> bool {
    matches!(
        command,
        cli::Command::Serve { .. }
            | cli::Command::CraftStub { .. }
            | cli::Command::Ctl { .. }
            | cli::Command::Diag { .. }
    )
}

/// The runtime a command needs, or the internal error that it is missing.
fn runtime_of(
    runtime: &Option<tokio::runtime::Runtime>,
) -> Result<&tokio::runtime::Runtime, Failure> {
    runtime
        .as_ref()
        .ok_or_else(|| Failure::new(Error::internal("command needs a runtime")))
}

struct Failure {
    error: Error,
    code: u8,
}

impl Failure {
    fn new(error: Error) -> Self {
        let code = match error.kind() {
            flowcraft::error::ErrorKind::Validation => 2,
            flowcraft::error::ErrorKind::NotFound => 3,
            _ => 1,
        };
        Self { error, code }
    }
}

fn run(args: Vec<String>) -> Result<ExitCode, Failure> {
    let (command, log_settings) = cli::parse(&args).map_err(Failure::new)?;

    // The runtime has to exist *before* the pipeline is installed: an OTLP
    // exporter spawns its batch worker when it is built, and there is no
    // runtime to spawn onto if this comes later. Entering it here also means a
    // `check` still runs on the calling thread.
    let runtime = if drives_a_runtime(&command) {
        Some(
            tokio::runtime::Builder::new_multi_thread()
                .enable_all()
                .build()
                .map_err(|error| Failure::new(Error::internal(error.to_string())))?,
        )
    } else {
        None
    };
    let _entered = runtime.as_ref().map(tokio::runtime::Runtime::enter);

    // Identity before telemetry: the pipeline bakes the resource fields in at
    // install time, so a record emitted before the identity is read would be
    // unattributable. Reading one small file is cheap next to parsing the
    // configuration, which comes later.
    let (identity, identity_error) = MachineIdentity::load_or_create();

    // Sinks and format are fixed when the pipeline is built, so the `log:`
    // section has to be known now. A document that fails to parse yields no
    // section; `serve` reports the real error through the pipeline it just got.
    let log_config = match &command {
        cli::Command::Serve { config, .. } => commands::peek_log(config),
        cli::Command::Check {
            config: Some(config),
            ..
        } => commands::peek_log(config),
        _ => None,
    };
    let applied = std::sync::Arc::new(
        match logging::install_for(&command, &log_settings, &identity, log_config.as_ref()) {
            Ok(applied) => applied,
            // Telemetry is an aid, not a prerequisite: a process that cannot log
            // still has work to do.
            Err(error) => {
                eprintln!("flowcraftd: telemetry unavailable: {error}");
                logging::Applied::disabled()
            }
        },
    );
    logging::report_identity(&identity, identity_error);

    match command {
        cli::Command::Help => {
            println!("{}", cli::usage_text());
            Ok(ExitCode::SUCCESS)
        }
        cli::Command::Version => {
            println!("flowcraftd {}", env!("CARGO_PKG_VERSION"));
            Ok(ExitCode::SUCCESS)
        }
        cli::Command::Catalog { json } => {
            let catalog = flowcraft::resource::Catalog::from_registry(&assembly::registry());
            if json {
                let text = serde_json::to_string_pretty(&catalog)
                    .map_err(|error| Failure::new(Error::internal(error.to_string())))?;
                println!("{text}");
            } else {
                for entry in &catalog.entries {
                    let name = match &entry.imp {
                        Some(imp) => format!("{}/{}", entry.kind, imp),
                        None => entry.kind.clone(),
                    };
                    println!("{name:<40} {:<6} {}", entry.phase, entry.build_type);
                }
            }
            Ok(ExitCode::SUCCESS)
        }
        cli::Command::Check {
            config,
            deploys,
            json,
        } => {
            let report = commands::check(config.as_deref(), &deploys).map_err(Failure::new)?;
            if json {
                let text = serde_json::to_string_pretty(&report)
                    .map_err(|error| Failure::new(Error::internal(error.to_string())))?;
                println!("{text}");
            } else if report.errors.is_empty() {
                println!("ok: {} resources planned", report.resources);
            } else {
                for error in &report.errors {
                    if error.path.is_empty() {
                        println!("{}", error.message);
                    } else {
                        println!("{}: {}", error.path, error.message);
                    }
                }
            }
            if report.errors.is_empty() {
                Ok(ExitCode::SUCCESS)
            } else {
                Ok(ExitCode::from(2))
            }
        }
        cli::Command::Ctl {
            socket,
            command,
            session,
        } => {
            runtime_of(&runtime)?
                .block_on(commands::ctl(&socket, &command, session.as_deref()))
                .map_err(Failure::new)?;
            Ok(ExitCode::SUCCESS)
        }
        cli::Command::CraftStub {
            target,
            tls_cert,
            tls_key,
            tls_client_ca,
            name,
            renew_log,
            token,
            abi_version,
            provides,
            bad_envelope,
        } => {
            runtime_of(&runtime)?
                .block_on(commands::craft_stub(
                    &commands::StubServe {
                        target,
                        cert: tls_cert,
                        key: tls_key,
                        client_ca: tls_client_ca,
                    },
                    &name,
                    renew_log.as_deref(),
                    token.as_deref(),
                    abi_version.as_deref(),
                    provides.as_deref(),
                    bad_envelope,
                ))
                .map_err(Failure::new)?;
            Ok(ExitCode::SUCCESS)
        }
        cli::Command::Diag { socket, out } => {
            runtime_of(&runtime)?
                .block_on(commands::diag(&socket, &out))
                .map_err(Failure::new)?;
            Ok(ExitCode::SUCCESS)
        }
        cli::Command::Serve { config, deploys } => {
            runtime_of(&runtime)?
                .block_on(commands::serve(&config, &deploys, applied.clone()))
                .map_err(Failure::new)?;
            applied.guard.shutdown();
            Ok(ExitCode::SUCCESS)
        }
    }
}

//! Command-line parsing.
//!
//! Hand-rolled rather than pulled from a crate: the surface is four commands
//! and the flag set is small enough that a dependency would cost more than it
//! saves.

use flowcraft::error::{Error, Result};

/// How much the operator wants to hear, and in what shape.
///
/// The flags are parsed with the command rather than separately: one pass over
/// `args`, one place that knows the spelling.
#[derive(Debug, Clone, Default)]
pub struct LogSettings {
    /// `--log-level <directives>`, in `tracing` filter syntax.
    pub level: Option<String>,
    /// `--log-format <text|json>`.
    pub format: Option<String>,
    /// `-v`/`--verbose`: debug.
    pub verbose: bool,
    /// `-q`/`--quiet`: warnings and errors only.
    pub quiet: bool,
    /// `--profile <service|client|cli>`: override the shape the command implies.
    pub profile: Option<String>,
    /// `--log-file <path>`: also write to a rotating file.
    pub file: Option<String>,
    /// `--log-ring`: keep recent output in memory for a support bundle.
    pub ring: bool,
}

impl LogSettings {
    /// Whether the operator said anything about the filter.
    ///
    /// `platform.yaml` must not override an explicit choice.
    #[must_use]
    pub fn provides_directives(&self) -> bool {
        self.level.is_some()
            || self.verbose
            || self.quiet
            || std::env::var("RUST_LOG")
                .map(|value| !value.is_empty())
                .unwrap_or(false)
    }
}

/// What the operator asked for.
pub enum Command {
    /// Print usage.
    Help,
    /// Print the version.
    Version,
    /// Print the capability catalog.
    Catalog {
        /// Emit JSON.
        json: bool,
    },
    /// Validate a host config and a deployment document.
    Check {
        /// The host config, when one is available.
        config: Option<String>,
        /// The deployment documents, merged in order.
        deploys: Vec<String>,
        /// Emit JSON.
        json: bool,
    },
    /// Send one command to a running daemon's admin socket.
    Ctl {
        /// Admin socket path.
        socket: String,
        /// `status`, `reload`, `drain`, `crafts`, or `revoke`.
        command: String,
        /// Session to revoke, for `revoke`.
        session: Option<String>,
    },
    /// Serve a minimal `craft.Craft/v1` endpoint (development aid).
    CraftStub {
        /// Socket to listen on.
        socket: String,
        /// Craft name reported by Describe.
        name: String,
        /// Append a line on every Renew (tests).
        renew_log: Option<String>,
        /// Require this bootstrap token on every call except Describe.
        token: Option<String>,
        /// Override the advertised ABI version (negative tests).
        abi_version: Option<String>,
        /// Advertise a provided capability (negative tests).
        provides: Option<String>,
        /// Emit a malformed envelope (negative tests).
        bad_envelope: bool,
    },
    /// Write a support bundle for a running daemon.
    Diag {
        /// Admin socket path.
        socket: String,
        /// Directory the bundle is written into.
        out: String,
    },
    /// Run the platform.
    Serve {
        /// The host config.
        config: String,
        /// The deployment documents, merged in order.
        deploys: Vec<String>,
    },
}

/// Parse `args` (without `argv[0]`).
pub fn parse(args: &[String]) -> Result<(Command, LogSettings)> {
    let Some((command, rest)) = args.split_first() else {
        return Err(usage("missing command"));
    };
    // `flowcraftd <cmd> --help` is the same as `flowcraftd --help`.
    if rest.iter().any(|arg| arg == "--help" || arg == "-h") {
        return Ok((Command::Help, LogSettings::default()));
    }
    match command.as_str() {
        "help" | "--help" | "-h" => Ok((Command::Help, LogSettings::default())),
        "version" | "--version" | "-V" => Ok((Command::Version, LogSettings::default())),
        "catalog" => {
            let flags = Flags::parse(rest)?;
            Ok((Command::Catalog { json: flags.json }, flags.log))
        }
        "check" => {
            let flags = Flags::parse(rest)?;
            if flags.deploys.is_empty() {
                return Err(usage("check needs --deploy <path>"));
            }
            Ok((
                Command::Check {
                    config: flags.config,
                    deploys: flags.deploys,
                    json: flags.json,
                },
                flags.log,
            ))
        }
        "ctl" => {
            let flags = Flags::parse(rest)?;
            let socket = flags
                .socket
                .ok_or_else(|| usage("ctl needs --socket <path>"))?;
            let command = flags
                .positional
                .ok_or_else(|| usage("ctl needs a command: status|reload|drain|crafts|revoke"))?;
            Ok((
                Command::Ctl {
                    socket,
                    command,
                    session: flags.session,
                },
                flags.log,
            ))
        }
        "craft-stub" => {
            let flags = Flags::parse(rest)?;
            let socket = flags
                .socket
                .ok_or_else(|| usage("craft-stub needs --socket <path>"))?;
            Ok((
                Command::CraftStub {
                    socket,
                    name: flags.name.unwrap_or_else(|| "stub".to_owned()),
                    renew_log: flags.renew_log,
                    token: flags.token,
                    abi_version: flags.abi_version,
                    provides: flags.provides,
                    bad_envelope: flags.bad_envelope,
                },
                flags.log,
            ))
        }
        "diag" => {
            let flags = Flags::parse(rest)?;
            let socket = flags
                .socket
                .ok_or_else(|| usage("diag needs --socket <path>"))?;
            let out = flags.out.ok_or_else(|| usage("diag needs --out <dir>"))?;
            Ok((Command::Diag { socket, out }, flags.log))
        }
        "serve" => {
            let flags = Flags::parse(rest)?;
            let config = flags
                .config
                .ok_or_else(|| usage("serve needs --config <path>"))?;
            if flags.deploys.is_empty() {
                return Err(usage("serve needs --deploy <path>"));
            }
            Ok((
                Command::Serve {
                    config,
                    deploys: flags.deploys,
                },
                flags.log,
            ))
        }
        other => Err(usage(format!("unknown command {other:?}"))),
    }
}

#[derive(Default)]
struct Flags {
    config: Option<String>,
    deploys: Vec<String>,
    socket: Option<String>,
    name: Option<String>,
    session: Option<String>,
    out: Option<String>,
    renew_log: Option<String>,
    token: Option<String>,
    abi_version: Option<String>,
    provides: Option<String>,
    bad_envelope: bool,
    positional: Option<String>,
    json: bool,
    log: LogSettings,
}

impl Flags {
    fn parse(args: &[String]) -> Result<Self> {
        let mut flags = Self::default();
        let mut index = 0;
        while index < args.len() {
            match args[index].as_str() {
                "--config" => {
                    flags.config = Some(value(args, index, "--config")?);
                    index += 2;
                }
                "--deploy" => {
                    // Repeatable: base first, then overlays.
                    flags.deploys.push(value(args, index, "--deploy")?);
                    index += 2;
                }
                "--socket" => {
                    flags.socket = Some(value(args, index, "--socket")?);
                    index += 2;
                }
                "--name" => {
                    flags.name = Some(value(args, index, "--name")?);
                    index += 2;
                }
                "--token" => {
                    flags.token = Some(value(args, index, "--token")?);
                    index += 2;
                }
                "--abi-version" => {
                    flags.abi_version = Some(value(args, index, "--abi-version")?);
                    index += 2;
                }
                "--provides" => {
                    flags.provides = Some(value(args, index, "--provides")?);
                    index += 2;
                }
                "--bad-envelope" => {
                    flags.bad_envelope = true;
                    index += 1;
                }
                "--renew-log" => {
                    flags.renew_log = Some(value(args, index, "--renew-log")?);
                    index += 2;
                }
                "--session" => {
                    flags.session = Some(value(args, index, "--session")?);
                    index += 2;
                }
                "--json" => {
                    flags.json = true;
                    index += 1;
                }
                "--log-level" => {
                    flags.log.level = Some(value(args, index, "--log-level")?);
                    index += 2;
                }
                "--log-format" => {
                    flags.log.format = Some(value(args, index, "--log-format")?);
                    index += 2;
                }
                "--log-file" => {
                    flags.log.file = Some(value(args, index, "--log-file")?);
                    index += 2;
                }
                "--log-ring" => {
                    flags.log.ring = true;
                    index += 1;
                }
                "--profile" => {
                    flags.log.profile = Some(value(args, index, "--profile")?);
                    index += 2;
                }
                "--out" => {
                    flags.out = Some(value(args, index, "--out")?);
                    index += 2;
                }
                "-v" | "--verbose" => {
                    flags.log.verbose = true;
                    index += 1;
                }
                "-q" | "--quiet" => {
                    flags.log.quiet = true;
                    index += 1;
                }
                other if !other.starts_with('-') && flags.positional.is_none() => {
                    flags.positional = Some(other.to_owned());
                    index += 1;
                }
                other => return Err(usage(format!("unknown flag {other:?}"))),
            }
        }
        Ok(flags)
    }
}

fn value(args: &[String], index: usize, flag: &str) -> Result<String> {
    args.get(index + 1)
        .cloned()
        .ok_or_else(|| usage(format!("{flag} needs a value")))
}

/// The usage text.
#[must_use]
pub const fn usage_text() -> &'static str {
    "usage:\n  flowcraftd serve --config <platform.yaml> --deploy <deploy.yaml> [--deploy <overlay.yaml>]...\n  flowcraftd check [--config <platform.yaml>] --deploy <deploy.yaml> [--deploy <overlay.yaml>]... [--json]\n  flowcraftd craft-stub --socket <path> [--name <name>]
  flowcraftd ctl --socket <path> <status|reload|drain|crafts|revoke> [--session <id>]\n  flowcraftd diag --socket <path> --out <dir>\n  flowcraftd catalog [--json]\n  flowcraftd version\n\nlog flags (any command):\n  --log-level <directives>  tracing filter syntax, e.g. \"info,flowcraft::kernel=debug\"\n  --log-format <text|json>  one record per line\n  --log-file <path>         also write to a rotating file\n  --log-ring                keep recent output in memory for `diag`\n  --profile <service|client|cli>  override the shape the command implies\n  -v, --verbose             shorthand for --log-level debug\n  -q, --quiet               warnings and errors only\n  RUST_LOG overrides --log-level"
}

fn usage(message: impl std::fmt::Display) -> Error {
    Error::validation(format!("{message}\n\n{}", usage_text()))
}

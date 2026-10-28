//! The in-sandbox bridge: the only process that can reach the proxy from inside a
//! network namespace.
//!
//! `bwrap`'s network isolation is all-or-nothing: `--unshare-net` gives an empty
//! namespace (loopback only) and `--share-net` inherits the host, which would make
//! the proxy a suggestion. A unix socket crosses the namespace boundary because it
//! is a filesystem object that can be bind-mounted in, but no HTTP client speaks
//! "proxy over a unix socket". So something inside the namespace must listen on
//! TCP and forward to that socket — and "something" has to be a process started by
//! `bwrap`'s argv.
//!
//! The bridge holds no rules and makes no decisions: it can only forward to the
//! bind-mounted socket, so compromising it yields no bypass, and killing it
//! removes the only egress. It also owns the three jobs only it can do:
//!
//! - it holds the TCP listener (the single hole), on an ephemeral port so no
//!   capability is needed;
//! - it injects the proxy environment, because the port is only known inside;
//! - it execs the real command and propagates its exit status, `128 + signal`
//!   for a signal death so a shell sees what it expects.
//!
//! **It is not a second artefact.** The host re-executes its own binary with
//! [`MARKER`] as the first argument (see [`run_if_invoked`]), so this crate ships
//! no bridge binary: `flowcraftd` is one such host, and a program that embeds the
//! sandbox capability makes itself one by calling [`run_if_invoked`] before it
//! looks at `argv`.

#[cfg(unix)]
use std::sync::Arc;

/// The reserved first argument that selects bridge mode.
pub const MARKER: &str = "--flowcraft-sandbox-bridge";

/// Where the proxy socket is bind-mounted inside a bwrap sandbox.
pub const PROXY_SOCKET: &str = "/run/flowcraft-proxy.sock";

/// Run the bridge if this process was re-executed as one.
///
/// Returns `false` for every normal invocation, so a host's `main` can simply
/// start with:
///
/// ```no_run
/// if flowcraft::caps::sandbox::net::bridge::run_if_invoked() {
///     return;
/// }
/// ```
///
/// # Panics
///
/// Exits the process on the bridge path, with the proxied command's status.
#[must_use]
pub fn run_if_invoked() -> bool {
    let argv: Vec<String> = std::env::args().collect();
    if argv.get(1).map(String::as_str) != Some(MARKER) {
        return false;
    }
    let status = run(&argv[2..]);
    std::process::exit(status);
}

/// Bridge mode: `--sock <path> -- <cmd> [args…]`, returning the exit code.
pub fn run(argv: &[String]) -> i32 {
    let mut sock = None;
    let mut command = Vec::new();
    let mut rest = argv.iter();
    while let Some(arg) = rest.next() {
        match arg.as_str() {
            "--sock" => sock = rest.next().cloned(),
            "--" => {
                command.extend(rest.cloned());
                break;
            }
            other => {
                eprintln!("bridge: unexpected argument {other:?}");
                return 2;
            }
        }
    }
    let (Some(sock), Some(program)) = (sock, command.first().cloned()) else {
        eprintln!("bridge: usage: --sock <path> -- <cmd> [args…]");
        return 2;
    };
    let args: Vec<String> = command.into_iter().skip(1).collect();

    let runtime = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(error) => {
            eprintln!("bridge: runtime: {error}");
            return 1;
        }
    };
    runtime.block_on(async move { serve(&sock, &program, &args).await })
}

#[cfg(unix)]
async fn serve(sock: &str, program: &str, args: &[String]) -> i32 {
    // Listen before starting the child: the port must exist when the command
    // first connects.
    let listener = match tokio::net::TcpListener::bind(("127.0.0.1", 0)).await {
        Ok(listener) => listener,
        Err(error) => {
            eprintln!("bridge: listen: {error}");
            return 1;
        }
    };
    let Ok(port) = listener.local_addr().map(|addr| addr.port()) else {
        return 1;
    };

    let mut child = match tokio::process::Command::new(program)
        .args(args)
        .env("HTTP_PROXY", format!("http://127.0.0.1:{port}"))
        .env("HTTPS_PROXY", format!("http://127.0.0.1:{port}"))
        .env("http_proxy", format!("http://127.0.0.1:{port}"))
        .env("https_proxy", format!("http://127.0.0.1:{port}"))
        .env("ALL_PROXY", format!("socks5h://127.0.0.1:{port}"))
        .env("all_proxy", format!("socks5h://127.0.0.1:{port}"))
        // `NO_PROXY` must be cleared: a deployment that leaves `*` behind would
        // send every connection straight past the only decision point.
        .env_remove("NO_PROXY")
        .env_remove("no_proxy")
        .spawn()
    {
        Ok(child) => child,
        Err(error) => {
            eprintln!("bridge: start {program}: {error}");
            return 127;
        }
    };

    let socket = Arc::new(sock.to_owned());
    tokio::spawn(async move {
        while let Ok((mut client, _)) = listener.accept().await {
            let socket = socket.clone();
            tokio::spawn(async move {
                let Ok(mut upstream) = tokio::net::UnixStream::connect(socket.as_str()).await
                else {
                    return;
                };
                let _ = tokio::io::copy_bidirectional(&mut client, &mut upstream).await;
            });
        }
    });

    match child.wait().await {
        Ok(status) => {
            #[cfg(unix)]
            {
                use std::os::unix::process::ExitStatusExt;
                if let Some(signal) = status.signal() {
                    return 128 + signal;
                }
            }
            status.code().unwrap_or(1)
        }
        Err(error) => {
            eprintln!("bridge: wait: {error}");
            127
        }
    }
}

/// Without unix sockets there is no socket to forward to, so the bridge has
/// nothing to be: a host re-executed as one says so instead of pretending.
#[cfg(not(unix))]
async fn serve(_sock: &str, _program: &str, _args: &[String]) -> i32 {
    eprintln!("bridge: the in-namespace bridge requires a unix socket target");
    2
}

#[cfg(test)]
mod tests {
    use super::*;

    fn argv(raw: &[&str]) -> Vec<String> {
        raw.iter().map(|arg| (*arg).to_owned()).collect()
    }

    #[test]
    fn a_malformed_invocation_is_rejected() {
        assert_eq!(run(&argv(&[])), 2);
        assert_eq!(run(&argv(&["--sock"])), 2);
        assert_eq!(run(&argv(&["--sock", "/tmp/x"])), 2);
        assert_eq!(run(&argv(&["--nonsense", "--", "true"])), 2);
    }

    #[test]
    fn a_missing_command_is_reported_like_a_shell_would() {
        let dir = tempfile::tempdir().expect("tempdir");
        let status = run(&argv(&[
            "--sock",
            dir.path().to_str().expect("utf8"),
            "--",
            "flowcraft-command-that-does-not-exist",
        ]));
        assert_eq!(status, 127);
    }

    #[test]
    fn the_command_runs_and_its_status_is_propagated() {
        let dir = tempfile::tempdir().expect("tempdir");
        // No proxy socket exists, but `true` never needs one.
        let status = run(&argv(&[
            "--sock",
            dir.path().join("absent.sock").to_str().expect("utf8"),
            "--",
            "sh",
            "-c",
            "exit 3",
        ]));
        assert_eq!(status, 3);
    }

    #[test]
    fn a_signal_death_becomes_one_hundred_and_twenty_eight_plus_the_signal() {
        let dir = tempfile::tempdir().expect("tempdir");
        let status = run(&argv(&[
            "--sock",
            dir.path().join("absent.sock").to_str().expect("utf8"),
            "--",
            "sh",
            "-c",
            "kill -TERM $$",
        ]));
        assert_eq!(status, 128 + 15);
    }
}

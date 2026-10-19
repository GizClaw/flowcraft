//! `core/hooks/exec_unix.go` and `exec_windows.go`: the platform half of a
//! timeout.
//!
//! A hook runs under `sh -c`, which is a wrapper: the work a timeout is meant to
//! stop is usually one level down, and killing the shell leaves it running. So
//! the command is started as the leader of a new process group and the group is
//! what gets killed, with SIGKILL rather than SIGTERM -- the hook is already over
//! budget, and a script that traps TERM would otherwise get a second grace period
//! it was never granted.
//!
//! Windows has neither process groups nor signals in this sense; core's
//! `exec_windows.go` kills the one process, and so does the port.

#[cfg(unix)]
pub(crate) fn own_process_group(command: &mut tokio::process::Command) {
    use std::os::unix::process::CommandExt;

    let _ = command.as_std_mut().process_group(0);
}

#[cfg(unix)]
pub(crate) fn kill(child: &mut tokio::process::Child) {
    use rustix::process::{Pid, Signal, kill_process_group};

    // The group id equals the leader's pid, because the command was started
    // with `process_group(0)`.
    let Some(pid) = child.id().and_then(|pid| Pid::from_raw(pid as i32)) else {
        let _ = child.start_kill();
        return;
    };
    if kill_process_group(pid, Signal::KILL).is_err() {
        // The group is already gone, or the platform refused: fall back to the
        // leader alone rather than reporting a hook as still running.
        let _ = child.start_kill();
    }
}

#[cfg(windows)]
pub(crate) fn own_process_group(_command: &mut tokio::process::Command) {}

#[cfg(windows)]
pub(crate) fn kill(child: &mut tokio::process::Child) {
    let _ = child.start_kill();
}

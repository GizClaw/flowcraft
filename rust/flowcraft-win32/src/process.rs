//! Starting a child with pipes, a token and a job — without `tokio::process`.
//!
//! `std`'s `Command` (and therefore `tokio::process`) creates the child itself, so
//! it cannot be given the two things a sandbox session needs: a **token** (the
//! write boundary, which can only be applied at `CreateProcessAsUser`) and a
//! **job** attached at creation (so no descendant is ever briefly uncontained).
//! This module does the creation, and hands the session the three pipe ends as
//! plain `File`s, which `tokio::fs` drives just as well as `tokio::process` does.
//!
//! Inheritance is the delicate part. `STARTF_USESTDHANDLES` requires
//! `bInheritHandles = TRUE`, and that would leak *every* inheritable handle in the
//! process. `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` narrows it to the three std
//! handles, which is the only reason this is safe to do inside a daemon that holds
//! sockets, endpoints and other sessions' pipes.

use std::ffi::c_void;

use windows_sys::Win32::Foundation::CloseHandle;
use windows_sys::Win32::Security::SECURITY_ATTRIBUTES;
use windows_sys::Win32::System::Threading::{
    CREATE_UNICODE_ENVIRONMENT, CreateProcessAsUserW, CreateProcessW, EXTENDED_STARTUPINFO_PRESENT,
    PROCESS_INFORMATION, STARTF_USESTDHANDLES, STARTUPINFOEXW, STARTUPINFOW,
};

use crate::job::Error;
use crate::pty::{
    AttributeList, Child, Launch, clear_inherit, command_line, create_pipe, environment_block,
    into_file, wide_path,
};

/// A started child and the session's ends of its three streams.
pub struct Spawned {
    child: Child,
    stdin: Option<std::fs::File>,
    stdout: Option<std::fs::File>,
    stderr: Option<std::fs::File>,
}

impl Spawned {
    /// The child, for waiting on and for its pid.
    pub fn child(&self) -> &Child {
        &self.child
    }

    /// What we write: goes to the child's stdin.
    pub fn take_stdin(&mut self) -> Option<std::fs::File> {
        self.stdin.take()
    }

    /// What the child wrote to stdout.
    pub fn take_stdout(&mut self) -> Option<std::fs::File> {
        self.stdout.take()
    }

    /// What the child wrote to stderr.
    pub fn take_stderr(&mut self) -> Option<std::fs::File> {
        self.stderr.take()
    }

    /// Give up the streams and keep the child, for a session that owns the wait.
    pub fn into_child(self) -> Child {
        self.child
    }
}

impl std::fmt::Debug for Spawned {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("flowcraft_win32::Spawned")
            .field("pid", &self.child.pid())
            .finish_non_exhaustive()
    }
}

/// Start `launch` with three pipes and no console.
///
/// The child's ends are inherited; ours are not, and the handle list makes sure
/// nothing else travels with them.
pub fn spawn(launch: Launch<'_>) -> Result<Spawned, Error> {
    let Launch {
        command,
        job,
        token,
    } = launch;

    // `create_pipe` returns `(read, write)` with both ends inheritable.
    let (stdin_child, stdin_ours) = create_pipe("sandbox: create the stdin pipe")?;
    let (stdout_ours, stdout_child) = create_pipe("sandbox: create the stdout pipe")?;
    let (stderr_ours, stderr_child) = create_pipe("sandbox: create the stderr pipe")?;
    // The ends we keep must not leak into the child, or a pipe would never reach
    // EOF while a session is alive.
    for end in [&stdin_ours, &stdout_ours, &stderr_ours] {
        clear_inherit(end, "sandbox: hide our end of a pipe from the child")?;
    }

    let mut attributes = AttributeList::new()?;
    if let Some(job) = job {
        attributes.set_job(job.handle())?;
    }
    // Last, as the API requires.
    attributes.set_handle_list(&[stdin_child.raw(), stdout_child.raw(), stderr_child.raw()])?;

    let mut info = STARTUPINFOEXW::default();
    info.StartupInfo.cb = u32::try_from(std::mem::size_of::<STARTUPINFOEXW>()).unwrap_or(u32::MAX);
    info.StartupInfo.dwFlags = STARTF_USESTDHANDLES;
    info.StartupInfo.hStdInput = stdin_child.raw();
    info.StartupInfo.hStdOutput = stdout_child.raw();
    info.StartupInfo.hStdError = stderr_child.raw();
    info.lpAttributeList = attributes.raw();

    let mut line = command_line(&command);
    let environment = command.env.map(environment_block);
    let cwd = command.cwd.map(wide_path);
    let mut process = PROCESS_INFORMATION::default();

    let application: *const u16 = std::ptr::null();
    let security: *const SECURITY_ATTRIBUTES = std::ptr::null();
    let environment_block = environment
        .as_ref()
        .map_or(std::ptr::null(), |block| block.as_ptr().cast::<c_void>());
    let directory = cwd.as_ref().map_or(std::ptr::null(), |dir| dir.as_ptr());
    let startup: *const STARTUPINFOW = &raw const info.StartupInfo;
    let out = &raw mut process;
    // SAFETY: the command line is a writable NUL-terminated buffer that outlives
    // the call; every pointer either points into a live binding or is null; the
    // child's three pipe ends are inheritable and named in the handle list, which
    // is what makes `bInheritHandles = TRUE` safe here; and the token, when
    // present, is a live primary token.
    let ok = unsafe {
        match token {
            Some(token) => CreateProcessAsUserW(
                token.raw(),
                application,
                line.as_mut_ptr(),
                security,
                std::ptr::null(),
                1,
                EXTENDED_STARTUPINFO_PRESENT | CREATE_UNICODE_ENVIRONMENT,
                environment_block,
                directory,
                startup,
                out,
            ),
            None => CreateProcessW(
                application,
                line.as_mut_ptr(),
                security,
                std::ptr::null(),
                1,
                EXTENDED_STARTUPINFO_PRESENT | CREATE_UNICODE_ENVIRONMENT,
                environment_block,
                directory,
                startup,
                out,
            ),
        }
    };
    if ok == 0 {
        return Err(Error::last(format!(
            "sandbox: cannot start {:?}",
            command.program
        )));
    }
    if !process.hThread.is_null() {
        // SAFETY: the handle came from `CreateProcess*` and is closed once.
        unsafe { CloseHandle(process.hThread) };
    }
    // The child owns its ends now; ours are closeable.
    drop(stdin_child);
    drop(stdout_child);
    drop(stderr_child);

    Ok(Spawned {
        child: Child::from_parts(process.dwProcessId, process.hProcess),
        stdin: Some(into_file(stdin_ours)),
        stdout: Some(into_file(stdout_ours)),
        stderr: Some(into_file(stderr_ours)),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pty::Command;
    use crate::{Integrity, Job, Limits, Token};
    use std::io::{Read, Write};

    /// A command whose arguments live long enough for the spawn.
    fn argv_spec(program: &str, args: &[String]) -> (String, Vec<String>) {
        (program.to_owned(), args.to_vec())
    }

    #[test]
    fn a_pipe_child_writes_to_stdout_and_exits_zero() {
        let (program, args) = argv_spec("cmd", &["/c".to_owned(), "echo pipe-works".to_owned()]);
        let mut spawned = spawn(Launch {
            command: Command {
                program: &program,
                args: &args,
                cwd: None,
                env: None,
            },
            job: None,
            token: None,
        })
        .expect("spawn");
        let mut stdout = spawned.take_stdout().expect("stdout");
        let code = spawned.child().wait().expect("wait");
        assert_eq!(code, 0);
        let mut text = String::new();
        let _ = stdout.read_to_string(&mut text);
        assert!(text.contains("pipe-works"), "{text:?}");
    }

    #[test]
    fn the_two_output_streams_stay_separate() {
        let (program, args) = argv_spec(
            "cmd",
            &["/c".to_owned(), "echo out & echo err 1>&2".to_owned()],
        );
        let mut spawned = spawn(Launch {
            command: Command {
                program: &program,
                args: &args,
                cwd: None,
                env: None,
            },
            job: None,
            token: None,
        })
        .expect("spawn");
        let mut stdout = spawned.take_stdout().expect("stdout");
        let mut stderr = spawned.take_stderr().expect("stderr");
        let _ = spawned.child().wait().expect("wait");
        let mut out = String::new();
        let mut err = String::new();
        let _ = stdout.read_to_string(&mut out);
        let _ = stderr.read_to_string(&mut err);
        assert!(out.contains("out"), "{out:?}");
        assert!(err.contains("err"), "{err:?}");
        assert!(!out.contains("err"), "the streams must not merge: {out:?}");
    }

    #[test]
    fn stdin_reaches_the_child() {
        let (program, args) = argv_spec(
            "cmd",
            // `%line%` expands before `set /p` runs, so the same line cannot read
            // it back; delayed expansion is what makes this test meaningful.
            &[
                "/v:on".to_owned(),
                "/c".to_owned(),
                "set /p line=& echo got:!line!".to_owned(),
            ],
        );
        let mut spawned = spawn(Launch {
            command: Command {
                program: &program,
                args: &args,
                cwd: None,
                env: None,
            },
            job: None,
            token: None,
        })
        .expect("spawn");
        let mut stdin = spawned.take_stdin().expect("stdin");
        let mut stdout = spawned.take_stdout().expect("stdout");
        stdin.write_all(b"hello\r\n").expect("write");
        stdin.flush().expect("flush");
        drop(stdin);
        let code = spawned.child().wait().expect("wait");
        assert_eq!(code, 0);
        let mut text = String::new();
        let _ = stdout.read_to_string(&mut text);
        assert!(text.contains("got:hello"), "{text:?}");
    }

    #[test]
    fn a_job_attached_at_creation_owns_the_tree() {
        let job = Job::create(Limits::default()).expect("job");
        let (program, args) = argv_spec(
            "cmd",
            &["/c".to_owned(), "ping -n 60 127.0.0.1 >NUL".to_owned()],
        );
        let spawned = spawn(Launch {
            command: Command {
                program: &program,
                args: &args,
                cwd: None,
                env: None,
            },
            job: Some(&job),
            token: None,
        })
        .expect("spawn");
        // The child is in the job from its first instruction, so a terminate
        // reaches it without an assignment race.
        job.terminate(0).expect("terminate");
        // A job terminate reports exit code 0, so the code says nothing; what
        // proves the kill is that a 60-second command returned at once.
        let started = std::time::Instant::now();
        let code = spawned
            .child()
            .wait_timeout(Some(std::time::Duration::from_secs(20)))
            .expect("wait");
        assert!(
            code.is_some(),
            "the job must have terminated the tree: {code:?}"
        );
        assert!(
            started.elapsed() < std::time::Duration::from_secs(20),
            "the child outlived the job: {:?}",
            started.elapsed()
        );
    }

    #[test]
    fn a_token_applies_to_a_pipe_child() {
        let token = Token::derive(Integrity::Low).expect("token");
        let (program, args) = argv_spec("cmd", &["/c".to_owned(), "whoami /groups".to_owned()]);
        let mut spawned = spawn(Launch {
            command: Command {
                program: &program,
                args: &args,
                cwd: None,
                env: None,
            },
            job: None,
            token: Some(&token),
        })
        .expect("spawn");
        let mut stdout = spawned.take_stdout().expect("stdout");
        let code = spawned.child().wait().expect("wait");
        assert_eq!(code, 0);
        let mut text = String::new();
        let _ = stdout.read_to_string(&mut text);
        assert!(
            text.contains("S-1-16-4096"),
            "a pipe child must carry the Low label too: {text:?}"
        );
    }

    /// A scratch directory under the system temp root.
    fn scratch(name: &str) -> std::path::PathBuf {
        let path = std::env::temp_dir().join(format!("flowcraft-{name}-{}", std::process::id()));
        std::fs::create_dir_all(&path).expect("scratch");
        path
    }

    fn run_in(dir: &std::path::Path, argv: &[String], close_stdin: bool) -> (i32, String) {
        let program = argv[0].clone();
        let args = argv[1..].to_vec();
        let mut spawned = spawn(Launch {
            command: Command {
                program: &program,
                args: &args,
                cwd: Some(dir),
                env: None,
            },
            job: None,
            token: None,
        })
        .expect("spawn");
        let mut stdout = spawned.take_stdout().expect("stdout");
        if close_stdin {
            // Exactly what the session does when the caller declared no input
            // side: the parent end goes away, and the child's stdin is at EOF
            // from its first read.
            drop(spawned.take_stdin());
        }
        let code = spawned
            .child()
            .wait_timeout(Some(std::time::Duration::from_secs(60)))
            .expect("wait")
            .expect("the child must exit");
        let mut text = String::new();
        let _ = stdout.read_to_string(&mut text);
        (code.cast_signed(), text)
    }

    #[test]
    fn the_working_directory_is_the_one_that_was_asked_for() {
        let dir = scratch("cwd");
        let (code, text) = run_in(
            &dir,
            &["cmd".to_owned(), "/c".to_owned(), "cd".to_owned()],
            false,
        );
        assert_eq!(code, 0, "{text:?}");
        let reported = text.trim().to_lowercase();
        assert_eq!(
            reported,
            dir.display().to_string().to_lowercase(),
            "the child must start in the directory it was given"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_child_with_a_closed_input_side_still_writes_to_its_directory() {
        // The shape the session uses for a non-interactive call: no input side,
        // and a file written relative to the working directory.
        let dir = scratch("noinput");
        let (code, text) = run_in(
            &dir,
            &[
                "cmd".to_owned(),
                "/c".to_owned(),
                "echo ok > marker.txt".to_owned(),
            ],
            true,
        );
        assert_eq!(code, 0, "{text:?}");
        let marker = dir.join("marker.txt");
        assert!(
            marker.is_file(),
            "the child must have written into its directory: {:?}",
            std::fs::read_dir(&dir).map(|entries| {
                entries
                    .filter_map(|entry| entry.ok().map(|e| e.file_name()))
                    .collect::<Vec<_>>()
            })
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_child_that_does_not_exist_is_an_error_not_a_panic() {
        let program = "flowcraft-no-such-program";
        let error = spawn(Launch {
            command: Command {
                program,
                args: &[],
                cwd: None,
                env: None,
            },
            job: None,
            token: None,
        })
        .expect_err("a missing program must fail");
        assert!(error.code.is_some(), "{error:?}");
    }
}

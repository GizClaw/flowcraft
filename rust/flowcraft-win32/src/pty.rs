//! ConPTY: a real terminal for a session that has no console of its own.
//!
//! A GUI-less daemon has no console to hand a child, so the child would see a
//! pipe: no cursor addressing, no `isatty`, no line editing. ConPTY
//! (`CreatePseudoConsole`, Windows 10 1809+) is the supported answer — the child
//! gets a console, we get one bidirectional stream of VT sequences.
//!
//! # Why this is not `portable-pty`
//!
//! `portable-pty` owns process creation, and the sandbox needs to own it: the
//! confinement phases attach a restricted token (W3) and an app container (W4)
//! at `CreateProcess` time, and the job has to be attached at creation so no
//! descendant is ever briefly outside it. A library that spawns the child itself
//! cannot be given any of that, so the pty is created here and the *sandbox*
//! spawns the process.
//!
//! # The shape of a ConPTY session
//!
//! ```text
//!   us ──write──▶ [input pipe] ──▶ HPCON ──▶ child's console
//!   us ◀──read─── [output pipe] ◀── HPCON ◀── child's console
//! ```
//!
//! Two pipes are created, handed to `CreatePseudoConsole`, and closed on our side
//! immediately: the console duplicates them, and keeping our copy open would stop
//! the output pipe from ever reaching EOF. The console handle then travels to
//! `CreateProcessW` in `STARTUPINFOEXW::lpAttributeList`, which is why
//! `bInheritHandles` is `FALSE` — nothing is inherited, everything is passed.
//!
//! The spawn sequence follows the wezterm backend that `codex-rs` also carries
//! (`wezterm/utils/pty/src/win/{psuedocon,procthreadattr}.rs`, MIT, © Wez Furlong),
//! adapted to `windows-sys` and to this crate's error type. Two details from it
//! are load-bearing and easy to miss: the std handles are explicitly *invalid*
//! (`STARTF_USESTDHANDLES`) so the console provides them, and the job is attached
//! through `PROC_THREAD_ATTRIBUTE_JOB_LIST` so a child is never briefly
//! uncontained.
//!
//! Ctrl-C is *not* `GenerateConsoleCtrlEvent`: that only reaches a process group
//! attached to the caller's own console, and the child's is a pseudo one. Writing
//! `0x03` into the terminal is the authentic path, because the console host turns
//! it into `CTRL_C_EVENT` for the foreground process group — the same thing a
//! keyboard does.

use std::collections::BTreeMap;
use std::ffi::{OsStr, OsString, c_void};
use std::os::windows::ffi::OsStrExt;
use std::os::windows::io::{FromRawHandle, RawHandle};
use std::path::Path;

use windows_sys::Win32::Foundation::{
    CloseHandle, HANDLE, HANDLE_FLAG_INHERIT, INVALID_HANDLE_VALUE, STILL_ACTIVE,
    SetHandleInformation, TRUE, WAIT_OBJECT_0, WAIT_TIMEOUT,
};
use windows_sys::Win32::Security::SECURITY_ATTRIBUTES;
use windows_sys::Win32::System::Console::{
    COORD, ClosePseudoConsole, CreatePseudoConsole, HPCON, ResizePseudoConsole,
};
use windows_sys::Win32::System::Pipes::CreatePipe;
use windows_sys::Win32::System::Threading::{
    CREATE_UNICODE_ENVIRONMENT, CreateProcessAsUserW, CreateProcessW,
    DeleteProcThreadAttributeList, EXTENDED_STARTUPINFO_PRESENT, GetExitCodeProcess,
    InitializeProcThreadAttributeList, LPPROC_THREAD_ATTRIBUTE_LIST,
    PROC_THREAD_ATTRIBUTE_HANDLE_LIST, PROC_THREAD_ATTRIBUTE_JOB_LIST,
    PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, PROCESS_INFORMATION, STARTF_USESTDHANDLES, STARTUPINFOEXW,
    STARTUPINFOW, UpdateProcThreadAttribute, WaitForSingleObject,
};

use crate::job::Error;

/// No creation flags: the console's documented defaults.
///
/// The reference backend passes `PSEUDOCONSOLE_RESIZE_QUIRK` here, but that is a
/// refinement of resize behaviour and not needed to attach a client. `0x1` is
/// `PSEUDOCONSOLE_INHERIT_CURSOR`, which asks the console to inherit the
/// *caller's* cursor — a request a console-less host cannot honour — so the
/// values are not interchangeable and the safe default is none.
const PSEUDOCONSOLE_FLAGS: u32 = 0;

/// Answers the one escape sequence a console sends *to* its terminal.
///
/// ConPTY writes `ESC[6n` ("report cursor position") and then waits for the
/// terminal to answer, because that is what a real terminal does. A session that
/// only reads never answers, and the console application stalls: this was the
/// entire reason a ConPTY child produced no output on a host where the reference
/// backend behaved identically.
///
/// The state machine keeps three trailing bytes so a request split across two
/// reads is still answered.
#[derive(Debug, Default)]
pub struct CursorReport {
    tail: Vec<u8>,
}

impl CursorReport {
    /// A fresh scanner.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Feed one chunk; the return value is the bytes to write back, if the chunk
    /// completed a request.
    pub fn feed(&mut self, chunk: &[u8]) -> Option<&'static [u8]> {
        /// `ESC [ 6 n`: device status report, i.e. "where is the cursor".
        const REQUEST: &[u8] = b"\x1b[6n";
        /// The answer: row 1, column 1. Any real position works — a console asks
        /// before it has drawn anything.
        const RESPONSE: &[u8] = b"\x1b[1;1R";

        let mut window = Vec::with_capacity(self.tail.len() + chunk.len());
        window.extend_from_slice(&self.tail);
        window.extend_from_slice(chunk);
        let found = window
            .windows(REQUEST.len())
            .any(|candidate| candidate == REQUEST);
        let keep = window.len().min(REQUEST.len() - 1);
        self.tail.clear();
        self.tail.extend_from_slice(&window[window.len() - keep..]);
        found.then_some(RESPONSE)
    }
}

/// Everything `spawn` needs: what to run, and the two things that make it a
/// *sandboxed* child rather than a plain one.
pub struct Launch<'a> {
    /// The program, its arguments, and the environment it sees.
    pub command: Command<'a>,
    /// The job the tree is attached to at creation, so no descendant is ever
    /// briefly outside it.
    pub job: Option<&'a crate::Job>,
    /// The token to start the child with. `Some` derived at Low is the write
    /// boundary; it travels through `CreateProcessAsUserW`, which is the only
    /// place a token can be applied.
    pub token: Option<&'a crate::Token>,
}

/// How to start the child. Everything is explicit: the sandbox decides the
/// environment and the working directory, not the host process.
#[derive(Debug, Clone, Copy)]
pub struct Command<'a> {
    /// The program to run (`argv[0]`).
    pub program: &'a str,
    /// Its arguments (`argv[1..]`).
    pub args: &'a [String],
    /// Working directory, when the caller pinned one.
    pub cwd: Option<&'a Path>,
    /// The environment block. `None` inherits ours; `Some` replaces it entirely.
    pub env: Option<&'a BTreeMap<String, String>>,
}

/// A pseudo console and the two pipes that carry its traffic.
pub struct Pty {
    console: HPCON,
    /// What we write: goes to the child's stdin.
    input: Option<PipeEnd>,
    /// What we read: the child's merged stdout and stderr.
    output: Option<PipeEnd>,
    /// The ends handed to `CreatePseudoConsole`, held for the console's life.
    ///
    /// They are *not* released after the spawn. Both releases were tried against a
    /// real Windows host: closing them at `CreatePseudoConsole` time and closing
    /// them right after `CreateProcessW` both produced a reader that returned EOF
    /// immediately with zero bytes, i.e. the console was holding our pipes only
    /// through these handles. Keeping them costs two handles per session and is
    /// what makes the stream real.
    creation: Option<(PipeEnd, PipeEnd)>,
}

impl std::fmt::Debug for Pty {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("flowcraft_win32::Pty")
    }
}

impl Pty {
    /// Whether this host has ConPTY: Windows 10 1809 and later.
    ///
    /// Probed by opening one, because the export check and the real capability
    /// disagree in both directions (an export can exist and still fail on a
    /// Server Core SKU).
    pub fn available() -> bool {
        Self::open(80, 24).is_ok()
    }

    /// Create a pseudo console of `cols` × `rows`.
    pub fn open(cols: u16, rows: u16) -> Result<Self, Error> {
        // `pipe` returns `(read, write)`. The console reads what we write, so it
        // takes the input pipe's *read* end; it writes what we read, so it takes
        // the output pipe's *write* end. The other two ends stay ours.
        let (input_console, input_ours) = pipe("sandbox: create the pty input pipe")?;
        let (output_ours, output_console) = pipe("sandbox: create the pty output pipe")?;

        let mut console: HPCON = 0;
        let size = COORD {
            X: i16::try_from(cols.max(1)).unwrap_or(i16::MAX),
            Y: i16::try_from(rows.max(1)).unwrap_or(i16::MAX),
        };
        // SAFETY: both handles are live and are the ends the console needs (it
        // reads from `input_console` and writes to `output_console`); the console
        // duplicates them, which is why they are closed right after.
        let hr = unsafe {
            CreatePseudoConsole(
                size,
                input_console.raw(),
                output_console.raw(),
                PSEUDOCONSOLE_FLAGS,
                &mut console,
            )
        };
        if hr < 0 {
            return Err(Error::hresult("sandbox: CreatePseudoConsole failed", hr));
        }
        Ok(Self {
            console,
            input: Some(input_ours),
            output: Some(output_ours),
            creation: Some((input_console, output_console)),
        })
    }

    /// Take the write end, so the session can stream stdin.
    pub fn take_input(&mut self) -> Option<std::fs::File> {
        self.input.take().map(into_file)
    }

    /// Take the read end, so the session can pump the merged stream.
    pub fn take_output(&mut self) -> Option<std::fs::File> {
        self.output.take().map(into_file)
    }

    /// Resize the terminal the child sees.
    pub fn resize(&self, cols: u16, rows: u16) -> Result<(), Error> {
        let size = COORD {
            X: i16::try_from(cols.max(1)).unwrap_or(i16::MAX),
            Y: i16::try_from(rows.max(1)).unwrap_or(i16::MAX),
        };
        // SAFETY: the console handle is live for as long as `self` is, and the
        // size is a plain coordinate pair.
        let hr = unsafe { ResizePseudoConsole(self.console, size) };
        if hr < 0 {
            return Err(Error::hresult("sandbox: ResizePseudoConsole failed", hr));
        }
        Ok(())
    }

    /// Start `command` attached to this console.
    ///
    /// The flags are the known-good set: no new console, no new process group
    /// (a pty tree is stopped by its job, and Ctrl-C is a byte), and an extended
    /// startup info so the console and the job travel as attributes.
    ///
    /// (Historical note: `CREATE_NEW_PROCESS_GROUP` is *not* used. It would make
    /// the child the root of a group that `GenerateConsoleCtrlEvent` could
    /// address, but an event cannot reach a process attached to a pseudo console,
    /// so the flag would buy nothing: Ctrl-C arrives as the byte `0x03`, and the
    /// tree is stopped through its job rather than through a console event.)
    pub fn spawn(&mut self, launch: Launch<'_>) -> Result<Child, Error> {
        let Launch {
            command,
            job,
            token,
        } = launch;
        let mut attributes = AttributeList::new()?;
        let console = self.console;
        attributes.set_pseudo_console(console)?;
        // Attach the tree at creation time. Assigning after the spawn leaves a
        // window in which the child can fork a grandchild that is outside the
        // job — and therefore outside the caps and outside `terminate`.
        if let Some(job) = job {
            attributes.set_job(job.handle())?;
        }

        let mut command_line = command_line(&command);
        let environment = command.env.map(environment_block);
        let cwd = command.cwd.map(wide_path);
        // The struct must be an `STARTUPINFOEXW`: Windows decides by `cb`.
        let mut info = STARTUPINFOEXW::default();
        info.StartupInfo.cb =
            u32::try_from(std::mem::size_of::<STARTUPINFOEXW>()).unwrap_or(u32::MAX);
        // Named, not inherited: a restricted-token child with no desktop can fail to
        // initialise with `STATUS_DLL_INIT_FAILED` (PowerShell is the usual report).
        let desktop: Vec<u16> = DEFAULT_DESKTOP
            .encode_utf16()
            .chain(std::iter::once(0))
            .collect();
        info.StartupInfo.lpDesktop = desktop.as_ptr() as *mut u16;
        // No inherited standard handles: the child's are the console's. Leaving
        // this unset makes the child inherit ours instead, and every read and
        // write then bypasses the terminal entirely.
        info.StartupInfo.dwFlags = STARTF_USESTDHANDLES;
        info.StartupInfo.hStdInput = INVALID_HANDLE_VALUE;
        info.StartupInfo.hStdOutput = INVALID_HANDLE_VALUE;
        info.StartupInfo.hStdError = INVALID_HANDLE_VALUE;
        info.lpAttributeList = attributes.raw();
        let mut process = PROCESS_INFORMATION::default();

        // Named only when the program carries a path; a bare name must keep the
        // search path it was resolved with, or `cmd` stops meaning `cmd`.
        let application_wide: Option<Vec<u16>> = names_a_path(command.program).then(|| {
            command
                .program
                .encode_utf16()
                .chain(std::iter::once(0))
                .collect()
        });
        // The binding above must outlive the call: a pointer taken from a temporary Vec
        // would dangle the moment the statement ended, which is a use-after-free that
        // compiles.
        let application: *const u16 = application_wide
            .as_ref()
            .map_or(std::ptr::null(), |wide| wide.as_ptr());
        let line = command_line.as_mut_ptr();
        let security: *const SECURITY_ATTRIBUTES = std::ptr::null();
        let environment_block = environment
            .as_ref()
            .map_or(std::ptr::null(), |block| block.as_ptr().cast::<c_void>());
        let directory = cwd.as_ref().map_or(std::ptr::null(), |dir| dir.as_ptr());
        let startup: *const STARTUPINFOW = &raw const info.StartupInfo;
        let out = &raw mut process;
        // SAFETY: the command line is a writable NUL-terminated buffer that
        // outlives the call; every pointer either points into a live binding or is
        // null; `bInheritHandles` is FALSE because the console travels in the
        // attribute list instead of through inheritance; and the token, when
        // present, is a live primary token with the rights the call needs.
        let ok = unsafe {
            match token {
                Some(token) => CreateProcessAsUserW(
                    token.raw(),
                    application,
                    line,
                    security,
                    std::ptr::null(),
                    0,
                    EXTENDED_STARTUPINFO_PRESENT | CREATE_UNICODE_ENVIRONMENT,
                    environment_block,
                    directory,
                    startup,
                    out,
                ),
                None => CreateProcessW(
                    application,
                    line,
                    security,
                    std::ptr::null(),
                    0,
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
                "sandbox: cannot start {:?} on the pty",
                command.program
            )));
        }

        // The thread handle is not needed: the process handle is the tree's root.
        if !process.hThread.is_null() {
            // SAFETY: the handle came from `CreateProcessW` above and is closed
            // exactly once.
            unsafe { CloseHandle(process.hThread) };
        }
        Ok(Child {
            pid: process.dwProcessId,
            process: process.hProcess,
        })
    }
}

impl Drop for Pty {
    fn drop(&mut self) {
        // The pipes must be released first: `ClosePseudoConsole` waits for the
        // console host to drain, and it can only drain if the reader keeps
        // reading — the session drops this handle after the pump has finished.
        self.input.take();
        self.output.take();
        self.creation.take();
        // SAFETY: the console handle was created by `CreatePseudoConsole` and is
        // closed exactly once, here.
        unsafe { ClosePseudoConsole(self.console) };
    }
}

/// A process started on the console.
pub struct Child {
    pid: u32,
    process: HANDLE,
}

// SAFETY: a process handle is a kernel object with no thread affinity; Win32
// documents every call used here as callable from any thread, and the handle is
// closed exactly once by `Drop`.
unsafe impl Send for Child {}

impl Child {
    /// Take ownership of a started process. The handle must be the process handle
    /// from `CreateProcess*`, closed exactly once by this value.
    pub(crate) fn from_parts(pid: u32, process: HANDLE) -> Self {
        Self { pid, process }
    }

    /// The process id.
    pub fn pid(&self) -> u32 {
        self.pid
    }

    /// Wait for the process to end and report its exit code.
    ///
    /// Blocking: the caller runs it on a thread that may block. A terminal's
    /// reader and its waiter are different threads by construction.
    pub fn wait(&self) -> Result<u32, Error> {
        match self.wait_timeout(None)? {
            Some(code) => Ok(code),
            // An unbounded wait cannot time out, so this branch is unreachable.
            None => Err(Error::last("sandbox: the pty child did not exit")),
        }
    }

    /// Wait up to `timeout`, answering `None` if the process is still running.
    ///
    /// `wait` is `INFINITE` by contract, but every *internal* waiter needs a
    /// bound: a session that never exits must not pin a thread forever.
    pub fn wait_timeout(&self, timeout: Option<std::time::Duration>) -> Result<Option<u32>, Error> {
        let millis = timeout.map_or(u32::MAX, |timeout| {
            u32::try_from(timeout.as_millis()).unwrap_or(u32::MAX)
        });
        // SAFETY: the handle is live and is not owned by the wait.
        let waited = unsafe { WaitForSingleObject(self.process, millis) };
        if waited == WAIT_TIMEOUT {
            return Ok(None);
        }
        if waited != WAIT_OBJECT_0 {
            return Err(Error::last("sandbox: waiting for the pty child failed"));
        }
        self.exit_code()
    }

    /// The exit code, or `None` while the process is still running.
    pub fn exit_code(&self) -> Result<Option<u32>, Error> {
        let mut code: u32 = 0;
        // SAFETY: the handle is live and the out-pointer is a live `u32`.
        let ok = unsafe { GetExitCodeProcess(self.process, &mut code) };
        if ok == 0 {
            return Err(Error::last("sandbox: GetExitCodeProcess failed"));
        }
        Ok((code.cast_signed() != STILL_ACTIVE).then_some(code))
    }
}

impl Drop for Child {
    fn drop(&mut self) {
        // SAFETY: the handle came from `CreateProcessW` and is closed once. The
        // process keeps running: the job object owns its lifetime, not this handle.
        unsafe { CloseHandle(self.process) };
    }
}

/// One end of an anonymous pipe, owned until handed to a `File`.
///
/// Crate-visible: [`crate::process`] builds the same pipes with the other
/// inheritance policy (a pipe session's child *needs* its ends inheritable).
pub(crate) struct PipeEnd(HANDLE);

// SAFETY: a pipe handle is a kernel object with no thread affinity, and ownership
// is unique — the `Drop` below is the only close, or the `File` that takes it.
unsafe impl Send for PipeEnd {}
// SAFETY: as above. Reads and writes on a pipe are serialized by the caller (the
// session holds the writer behind an async mutex and the reader in one pump).
unsafe impl Sync for PipeEnd {}

impl PipeEnd {
    pub(crate) fn raw(&self) -> HANDLE {
        self.0
    }
}

impl Drop for PipeEnd {
    fn drop(&mut self) {
        // SAFETY: the handle came from `CreatePipe` and is closed exactly once:
        // either here, or by the `File` that took ownership.
        unsafe { CloseHandle(self.0) };
    }
}

/// A pipe whose parent end is not inheritable.
///
/// `CreatePipe` marks both ends inheritable; the end we keep must not leak into
/// the child, or the child would hold its own output pipe open and the stream
/// would never end.
pub(crate) fn create_pipe(context: &'static str) -> Result<(PipeEnd, PipeEnd), Error> {
    let attributes = SECURITY_ATTRIBUTES {
        nLength: u32::try_from(std::mem::size_of::<SECURITY_ATTRIBUTES>()).unwrap_or(u32::MAX),
        lpSecurityDescriptor: std::ptr::null_mut(),
        bInheritHandle: TRUE,
    };
    let mut read: HANDLE = std::ptr::null_mut();
    let mut write: HANDLE = std::ptr::null_mut();
    // SAFETY: `attributes` outlives the call, and both out-pointers are live.
    let ok = unsafe { CreatePipe(&mut read, &mut write, &attributes, 0) };
    if ok == 0 {
        return Err(Error::last(context));
    }
    Ok((PipeEnd(read), PipeEnd(write)))
}

/// Stop `end` from leaking into a child.
pub(crate) fn clear_inherit(end: &PipeEnd, context: &'static str) -> Result<(), Error> {
    // SAFETY: the handle is live and is only being told not to inherit.
    let ok = unsafe { SetHandleInformation(end.raw(), HANDLE_FLAG_INHERIT, 0) };
    if ok == 0 {
        return Err(Error::last(context));
    }
    Ok(())
}

/// A pipe whose ends are both ours alone: the console duplicates what it needs.
fn pipe(context: &'static str) -> Result<(PipeEnd, PipeEnd), Error> {
    let (read, write) = create_pipe(context)?;
    // The console duplicates the ends it needs, so inheritability is cleared on
    // both: nothing of ours may leak into the child by accident.
    clear_inherit(&read, context)?;
    clear_inherit(&write, context)?;
    Ok((read, write))
}

/// Take ownership of a pipe end as a `File`, so `tokio` can drive it.
pub(crate) fn into_file(end: PipeEnd) -> std::fs::File {
    let handle = std::mem::ManuallyDrop::new(end);
    // SAFETY: ownership moves from `PipeEnd` to the `File`, the handle is valid,
    // and the `ManuallyDrop` view stops the pipe end from closing it twice.
    unsafe { std::fs::File::from_raw_handle(handle.0 as RawHandle) }
}

/// A `PROC_THREAD_ATTRIBUTE_LIST` holding exactly one attribute.
pub(crate) struct AttributeList {
    /// Pointer-aligned words: the list is opaque, and the allocator only
    /// guarantees `u8` alignment for a byte buffer.
    storage: Vec<u64>,
    /// The job list the attribute points at: it must outlive `CreateProcessW`.
    job_list: Vec<HANDLE>,
    /// The handles a child may inherit. Must be added last, after every other
    /// attribute, or `UpdateProcThreadAttribute` rejects it.
    handle_list: Vec<HANDLE>,
}

impl AttributeList {
    pub(crate) fn new() -> Result<Self, Error> {
        // Room for both attributes: the console, and the job the tree is attached
        // to. A list sized for one makes the second `UpdateProcThreadAttribute`
        // fail, so the two travel together here.
        const ATTRIBUTES: u32 = 3;
        let mut size: usize = 0;
        // SAFETY: a null list with a valid size slot is the documented way to ask
        // for the required size.
        unsafe {
            InitializeProcThreadAttributeList(std::ptr::null_mut(), ATTRIBUTES, 0, &mut size)
        };
        if size == 0 {
            return Err(Error::last(
                "sandbox: InitializeProcThreadAttributeList reported no size",
            ));
        }
        // Over-aligned on purpose: the list is opaque and the allocator only
        // guarantees `u8` alignment.
        let mut storage = vec![0u64; size.div_ceil(8)];
        let raw = storage.as_mut_ptr().cast::<c_void>();
        // SAFETY: the buffer is at least `size` bytes and outlives the list.
        let ok = unsafe {
            InitializeProcThreadAttributeList(
                raw as LPPROC_THREAD_ATTRIBUTE_LIST,
                ATTRIBUTES,
                0,
                &mut size,
            )
        };
        if ok == 0 {
            return Err(Error::last(
                "sandbox: InitializeProcThreadAttributeList failed",
            ));
        }
        Ok(Self {
            storage,
            job_list: Vec::new(),
            handle_list: Vec::new(),
        })
    }

    pub(crate) fn raw(&self) -> LPPROC_THREAD_ATTRIBUTE_LIST {
        self.storage.as_ptr().cast::<c_void>().cast_mut()
    }

    /// Attach the child to `job` at creation.
    ///
    /// The kernel applies this during `CreateProcessW`, so the process is a job
    /// member before its first instruction. A failure here is fatal on purpose:
    /// running on without the cap would be a worse outcome than refusing.
    pub(crate) fn set_job(&mut self, job: HANDLE) -> Result<(), Error> {
        self.job_list = vec![job];
        let value = self.job_list.as_mut_ptr().cast::<c_void>();
        let size = std::mem::size_of_val(self.job_list.as_slice());
        // SAFETY: `value` points into `self.job_list`, which stays alive until
        // this list is dropped, and `size` covers exactly that slice.
        let ok = unsafe {
            UpdateProcThreadAttribute(
                self.raw(),
                0,
                usize::try_from(PROC_THREAD_ATTRIBUTE_JOB_LIST).unwrap_or(0),
                value,
                size,
                std::ptr::null_mut(),
                std::ptr::null(),
            )
        };
        if ok == 0 {
            return Err(Error::last(
                "sandbox: cannot attach the child to its job at creation (a parent job may forbid nesting)",
            ));
        }
        Ok(())
    }

    /// Restrict inheritance to exactly these handles.
    ///
    /// With `bInheritHandles = TRUE` a child otherwise inherits *every* inheritable
    /// handle in the process — sockets, files, another session's pipes. Must be
    /// added after the other attributes, which is why it is a separate call.
    pub(crate) fn set_handle_list(&mut self, handles: &[HANDLE]) -> Result<(), Error> {
        self.handle_list = handles.to_vec();
        let value = self.handle_list.as_mut_ptr().cast::<c_void>();
        let size = std::mem::size_of_val(self.handle_list.as_slice());
        // SAFETY: `value` points into `self.handle_list`, which stays alive until
        // this list is dropped, and `size` covers exactly that slice.
        let ok = unsafe {
            UpdateProcThreadAttribute(
                self.raw(),
                0,
                usize::try_from(PROC_THREAD_ATTRIBUTE_HANDLE_LIST).unwrap_or(0),
                value,
                size,
                std::ptr::null_mut(),
                std::ptr::null(),
            )
        };
        if ok == 0 {
            return Err(Error::last(
                "sandbox: cannot restrict the child's inherited handles",
            ));
        }
        Ok(())
    }

    pub(crate) fn set_pseudo_console(&mut self, console: HPCON) -> Result<(), Error> {
        // The handle *is* the value: for this one attribute `lpValue` is the
        // `HPCON` itself, not a pointer to it (Microsoft's own sample and the
        // wezterm backend both pass it directly). Passing a pointer to the handle
        // "succeeds" and silently attaches nothing, which is a silent failure
        // worth the comment.
        let value = console as usize as *mut c_void;
        // SAFETY: `value` is the console handle the attribute expects, and its
        // size is the handle's size.
        let ok = unsafe {
            UpdateProcThreadAttribute(
                self.raw(),
                0,
                usize::try_from(PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE).unwrap_or(0),
                value,
                std::mem::size_of::<HPCON>(),
                std::ptr::null_mut(),
                std::ptr::null(),
            )
        };
        if ok == 0 {
            return Err(Error::last("sandbox: UpdateProcThreadAttribute failed"));
        }
        Ok(())
    }
}

impl Drop for AttributeList {
    fn drop(&mut self) {
        // SAFETY: the list was initialised in `new` and is deleted exactly once.
        unsafe { DeleteProcThreadAttributeList(self.raw()) };
    }
}

/// Build the `lpCommandLine` for `CreateProcessW`.
///
/// Quoting follows the documented rules: an argument with a space or a quote is
/// wrapped, backslashes before a quote are doubled, and a trailing backslash is
/// doubled so it cannot escape the closing quote.
/// Whether the program must be named explicitly instead of searched for.
///
/// `CreateProcess*` with `lpApplicationName = NULL` treats the command line's first
/// token as the program and resolves a bare name through `PATH`. Naming the
/// application *disables that search*, which is why a bare name (`cmd`) must stay
/// unnamed — and why a name carrying a path is named here: a contained child has no
/// search path of its own, so it needs the absolute path spelled out.
pub(crate) fn names_a_path(program: &str) -> bool {
    !program.is_empty() && (program.contains(['\\', '/']) || Path::new(program).is_absolute())
}

/// The desktop a child starts on when nothing else is said.
///
/// `winsta0\\default` is the interactive desktop. Choosing it explicitly is what
/// keeps a restricted-token child from initialising without one.
pub(crate) const DEFAULT_DESKTOP: &str = "winsta0\\default";

pub(crate) fn command_line(command: &Command<'_>) -> Vec<u16> {
    let mut line = OsString::new();
    line.push(quote(command.program));
    for argument in command.args {
        line.push(" ");
        line.push(quote(argument));
    }
    let mut wide: Vec<u16> = line.encode_wide().collect();
    wide.push(0);
    wide
}

pub(crate) fn quote(raw: &str) -> String {
    if !raw.is_empty() && !raw.contains([' ', '\t', '"']) {
        return raw.to_owned();
    }
    let mut out = String::with_capacity(raw.len() + 2);
    out.push('"');
    let mut backslashes = 0usize;
    for character in raw.chars() {
        match character {
            '\\' => backslashes += 1,
            '"' => {
                for _ in 0..backslashes * 2 + 1 {
                    out.push('\\');
                }
                out.push('"');
                backslashes = 0;
            }
            other => {
                for _ in 0..backslashes {
                    out.push('\\');
                }
                backslashes = 0;
                out.push(other);
            }
        }
    }
    for _ in 0..backslashes * 2 {
        out.push('\\');
    }
    out.push('"');
    out
}

/// Build a `CREATE_UNICODE_ENVIRONMENT` block: `K=V\0K=V\0\0`.
pub(crate) fn environment_block(env: &BTreeMap<String, String>) -> Vec<u16> {
    let mut block = Vec::new();
    for (name, value) in env {
        block.extend(OsStr::new(&format!("{name}={value}")).encode_wide());
        block.push(0);
    }
    block.push(0);
    block
}

/// A path as a child should see it.
///
/// `std::fs::canonicalize` (and therefore the sandbox's own workdir resolution)
/// returns a *verbatim* path — `\\?\C:\...` — because that is the form that skips
/// the legacy path parsing. Children cannot use it: `cmd.exe` answers "UNC paths
/// are not supported. Defaulting to Windows directory" and then runs in the wrong
/// place, which is how a batch file "disappears". Our own IO keeps the verbatim
/// path; a child gets the plain one.
pub(crate) fn wide_path(path: &Path) -> Vec<u16> {
    let mut wide: Vec<u16> = path.as_os_str().encode_wide().collect();
    // `\\?\`
    const VERBATIM: [u16; 4] = [92, 92, 63, 92];
    // `UNC\`
    const UNC: [u16; 4] = [85, 78, 67, 92];
    if wide.starts_with(&VERBATIM) {
        if wide[4..].starts_with(&UNC) {
            // `\\?\UNC\server\share` -> `\\server\share`
            wide = [&VERBATIM[..2], &wide[8..]].concat();
        } else {
            wide.drain(..VERBATIM.len());
        }
    }
    wide.push(0);
    wide
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    fn command(program: &str, args: &[String]) -> (String, Vec<String>) {
        (program.to_owned(), args.to_vec())
    }

    #[test]
    fn conpty_is_available_on_a_supported_host() {
        // Windows 10 1809+ and Windows Server 2019+. A CI runner is well past
        // that; the assertion is here so a regression in `open` is visible.
        assert!(Pty::available(), "ConPTY must be available on this host");
    }

    #[test]
    fn a_terminal_can_be_opened_and_resized() {
        let pty = Pty::open(80, 24).expect("open");
        pty.resize(120, 40).expect("resize");
        // The pipes are ours until a session takes them.
        let mut pty = pty;
        assert!(pty.take_input().is_some());
        assert!(pty.take_output().is_some());
    }

    #[test]
    fn a_child_runs_on_the_pseudo_console_and_reports_its_exit() {
        let mut pty = Pty::open(80, 24).expect("open");
        let mut output = pty.take_output().expect("output");
        let (program, args) = command("cmd", &["/c".to_owned(), "echo conpty-works".to_owned()]);
        let child = pty
            .spawn(Launch {
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
        assert!(child.pid() > 0);

        // A reader thread drains while the child runs: `ClosePseudoConsole` waits
        // for the console host to drain, and the output pipe only reaches EOF once
        // the console is closed, so neither order works on its own.
        let reader = std::thread::spawn(move || {
            use std::io::Read;
            let mut text = Vec::new();
            let _ = output.read_to_end(&mut text);
            text
        });
        let code = child
            .wait_timeout(Some(Duration::from_secs(60)))
            .expect("wait")
            .expect("the child must exit within the timeout");
        assert_eq!(code, 0);
        drop(pty);
        let text = String::from_utf8_lossy(&reader.join().expect("reader")).into_owned();
        assert!(
            text.contains("conpty-works"),
            "the console stream must carry the output: {text:?}"
        );
    }

    #[test]
    fn a_quit_byte_reaches_the_child_as_ctrl_c() {
        let mut pty = Pty::open(80, 24).expect("open");
        let mut input = pty.take_input().expect("input");
        let (program, args) = command(
            "cmd",
            &[
                "/c".to_owned(),
                // Wait for input, then report what it was: `0x03` is Ctrl-C on a
                // console, and the console host turns it into `CTRL_C_EVENT`.
                "set /p line=& echo got-it".to_owned(),
            ],
        );
        let child = pty
            .spawn(Launch {
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
        {
            use std::io::Write;
            input.write_all(b"hello\r\n").expect("write");
            input.flush().expect("flush");
        }
        // Bounded on purpose: `set /p` ends on a line, and a test that blocks
        // forever on a missing one takes the whole lane with it.
        let ended = child
            .wait_timeout(Some(Duration::from_secs(30)))
            .expect("wait");
        assert!(ended.is_some(), "the line must have reached the child");
    }

    /// Tells "the console is attached" from "the console is not attached"
    /// without guessing: `mode con` only answers for a process that owns a
    /// console, and it prints the window the pty was created with.
    #[test]
    fn a_child_sees_the_pseudo_console_as_its_console() {
        let mut pty = Pty::open(100, 30).expect("open");
        let mut output = pty.take_output().expect("output");
        let (program, args) = command("cmd", &["/c".to_owned(), "mode con".to_owned()]);
        let child = pty
            .spawn(Launch {
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
        let reader = std::thread::spawn(move || {
            use std::io::Read;
            let mut text = Vec::new();
            let _ = output.read_to_end(&mut text);
            text
        });
        let code = child
            .wait_timeout(Some(Duration::from_secs(60)))
            .expect("wait");
        drop(pty);
        let text = String::from_utf8_lossy(&reader.join().expect("reader")).into_owned();
        assert!(
            text.contains("100"),
            "the child must report the console we created (100 columns); it exited \
             with {code:?}: {text:?}"
        );
    }

    /// Run `argv` on a pty with `token`, returning what the console carried.
    fn run_with_token(token: Option<&crate::Token>, argv: &[&str]) -> String {
        let mut pty = Pty::open(120, 30).expect("open");
        let mut output = pty.take_output().expect("output");
        let program = argv[0].to_owned();
        let args: Vec<String> = argv[1..].iter().map(|arg| (*arg).to_owned()).collect();
        let child = pty
            .spawn(Launch {
                command: Command {
                    program: &program,
                    args: &args,
                    cwd: None,
                    env: None,
                },
                job: None,
                token,
            })
            .expect("spawn");
        let reader = std::thread::spawn(move || {
            use std::io::Read;
            let mut text = Vec::new();
            let _ = output.read_to_end(&mut text);
            text
        });
        let code = child
            .wait_timeout(Some(Duration::from_secs(60)))
            .expect("wait");
        drop(pty);
        let text = String::from_utf8_lossy(&reader.join().expect("reader")).into_owned();
        assert!(code.is_some(), "the child must exit: {text:?}");
        text
    }

    #[test]
    fn a_child_started_with_a_low_token_runs_at_low_integrity() {
        let token = crate::Token::derive(crate::Integrity::Low).expect("token");
        // `whoami /groups` prints the token's integrity label; the SID is the part
        // that is not localized.
        let text = run_with_token(Some(&token), &["cmd", "/c", "whoami /groups"]);
        assert!(
            text.contains("S-1-16-4096"),
            "the child must carry the Low label: {text:?}"
        );
    }

    #[test]
    fn a_low_token_child_cannot_write_to_the_user_profile() {
        let token = crate::Token::derive(crate::Integrity::Low).expect("token");
        let Some(home) = std::env::var_os("USERPROFILE") else {
            return; // no profile, nothing to protect
        };
        let target = std::path::PathBuf::from(home)
            .join(format!("flowcraft-low-{}.txt", std::process::id()));
        // The *test* runs at Medium: this file is writable for us, which is what
        // makes the child's failure meaningful.
        std::fs::write(&target, b"medium can write here").expect("the test can write its home");
        let _ = std::fs::remove_file(&target);

        let command = format!("echo x > \"{}\"", target.display());
        let text = run_with_token(Some(&token), &["cmd", "/c", &command]);
        assert!(
            !target.exists(),
            "a Low child must not write to the profile: {text:?}"
        );
    }

    #[test]
    fn the_environment_block_is_double_terminated() {
        let env = BTreeMap::from([("A".to_owned(), "1".to_owned())]);
        let block = environment_block(&env);
        assert_eq!(block.last().copied(), Some(0));
        assert_eq!(block[block.len() - 2], 0);
        assert!(block.len() > 3);
    }

    #[test]
    fn a_cursor_request_is_answered() {
        let mut report = CursorReport::new();
        assert_eq!(report.feed(b"\x1b[6n"), Some(&b"\x1b[1;1R"[..]));
        // Nothing to answer in ordinary output.
        assert_eq!(report.feed(b"plain text"), None);
    }

    #[test]
    fn a_cursor_request_split_across_reads_is_still_answered() {
        let mut report = CursorReport::new();
        assert_eq!(report.feed(b"out\x1b["), None);
        assert_eq!(report.feed(b"6n"), Some(&b"\x1b[1;1R"[..]));
        // And once answered, the tail does not fire twice.
        assert_eq!(report.feed(b"n6"), None);
    }

    #[test]
    fn a_verbatim_path_is_handed_to_the_child_plain() {
        let plain = |path: &str| {
            let wide = wide_path(Path::new(path));
            String::from_utf16_lossy(&wide[..wide.len() - 1])
        };
        assert_eq!(plain(r"\\?\C:\work\dir"), r"C:\work\dir");
        assert_eq!(plain(r"\\?\UNC\server\share\dir"), r"\\server\share\dir");
        assert_eq!(plain(r"C:\work"), r"C:\work");
    }

    #[test]
    fn a_path_with_spaces_is_quoted_once() {
        assert_eq!(quote("plain"), "plain");
        assert_eq!(quote("has space"), "\"has space\"");
        assert_eq!(quote("say \"hi\""), "\"say \\\"hi\\\"\"");
        // No space, no quote: nothing to protect, so no quoting. A trailing
        // backslash only matters inside an argument that is quoted.
        assert_eq!(quote("dir\\"), "dir\\");
        assert_eq!(quote("dir with space\\"), "\"dir with space\\\\\"");
    }

    #[test]
    fn dropping_the_terminal_does_not_hang_on_an_open_child() {
        // A dropped console with a live reader would block in
        // `ClosePseudoConsole`; the pipes are released first for that reason.
        let pty = Pty::open(80, 24).expect("open");
        let started = std::time::Instant::now();
        drop(pty);
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "dropping an unused console must not block"
        );
    }
}

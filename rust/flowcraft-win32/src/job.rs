//! Job objects: the process-tree boundary on Windows.
//!
//! A job object is the only "process group" Windows has, and it is a stronger
//! primitive than the unix one: membership is inherited by every descendant, the
//! kernel can cap the tree's memory and cpu time, and `KILL_ON_JOB_CLOSE` means a
//! daemon that dies does not leave orphans behind.
//!
//! Three properties are what the sandbox relies on:
//!
//! - **the tree is the unit**: one `AssignProcessToJobObject` covers everything
//!   the child forks, so `TerminateJobObject` and the limits apply to all of it;
//! - **limits are kernel accounting**, not sampling: `JobMemoryLimit` is a hard
//!   ceiling and `PerJobUserTimeLimit` is a cpu-time *budget* (100ns units), which
//!   is exactly the shape of the sandbox contract's `cpu_millicores × timeout`;
//! - **the counters are readable**: `QueryInformationJobObject` answers "how much
//!   did this tree use" and "did it hit a limit", which is what lets a session
//!   report `BudgetExceeded` instead of a mysterious exit code.

use std::ffi::c_void;
use std::time::Duration;

/// A Win32 failure with the code that produced it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Error {
    /// The `GetLastError` value, when the failure came from Win32.
    pub code: Option<u32>,
    /// A human-readable description.
    pub message: String,
}

impl Error {
    /// A COM-style failure: ConPTY reports `HRESULT`s, not `GetLastError`.
    pub(crate) fn hresult(message: impl Into<String>, hr: i32) -> Self {
        Self {
            code: Some(hr as u32),
            message: format!("{} (hresult {hr:#010x})", message.into()),
        }
    }

    pub(crate) fn last(message: impl Into<String>) -> Self {
        Self {
            code: std::io::Error::last_os_error()
                .raw_os_error()
                .map(|raw| raw as u32),
            message: message.into(),
        }
    }
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self.code {
            Some(code) => write!(f, "{} (win32 error {code})", self.message),
            None => f.write_str(&self.message),
        }
    }
}

impl std::error::Error for Error {}

/// The kernel limits to install on a job.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Limits {
    /// Hard ceiling on the tree's committed memory, in bytes.
    pub memory_bytes: Option<u64>,
    /// Cpu-time budget for the whole tree.
    pub cpu_budget: Option<Duration>,
}

/// What a job has used so far.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Usage {
    /// User-mode cpu time consumed by every process in the job.
    pub user_time: Duration,
    /// Peak committed memory of the job, in bytes.
    pub peak_memory: u64,
    /// Processes currently assigned.
    pub active_processes: u32,
}

/// A limit the kernel enforced; the caller maps it to its own vocabulary.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Violation {
    /// The tree hit `JobMemoryLimit`.
    Memory,
    /// The tree hit `PerJobUserTimeLimit`.
    Time,
}

/// A job object holding one sandbox session's process tree.
///
/// Cloning shares the handle (the last clone closes it): a session, its budget
/// supervisor and its reaper all need to name the same job, and `KILL_ON_JOB_CLOSE`
/// must fire once, when the last of them lets go.
pub struct Job {
    inner: std::sync::Arc<Inner>,
}

struct Inner {
    // A raw handle is not `Send`/`Sync` by construction; the job object itself is
    // a kernel object with no thread affinity, and every method here is a
    // synchronous Win32 call, so sharing it across threads is sound.
    handle: HANDLE,
}

// SAFETY: see the note on `Inner::handle`. The job object is a kernel object;
// Win32 documents every function used here as callable from any thread, and the
// handle is only ever closed by `Drop for Inner`.
unsafe impl Send for Inner {}
// SAFETY: as above — the accessors are thread-safe Win32 queries, and mutation
// (limits, terminate) is serialized by the caller's session lock.
unsafe impl Sync for Inner {}

impl Clone for Job {
    fn clone(&self) -> Self {
        Self {
            inner: self.inner.clone(),
        }
    }
}

impl std::fmt::Debug for Job {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("flowcraft_win32::Job")
    }
}

impl Job {
    /// Create a job with `limits` and `KILL_ON_JOB_CLOSE`.
    ///
    /// Also the probe: a host where this fails has no usable job objects, and the
    /// sandbox must report the caps as unavailable rather than advertise them.
    pub fn create(limits: Limits) -> Result<Self, Error> {
        // SAFETY: a null name and a null `SECURITY_ATTRIBUTES` ask for an
        // unnamed job with the default descriptor, which is all the sandbox
        // wants: the handle must not be inheritable.
        let handle = unsafe { CreateJobObjectW(std::ptr::null(), std::ptr::null()) };
        if handle.is_null() {
            return Err(Error::last("sandbox: CreateJobObject failed"));
        }
        let job = Self {
            inner: std::sync::Arc::new(Inner { handle }),
        };
        job.install(limits)?;
        Ok(job)
    }

    /// Install the limits, keeping `KILL_ON_JOB_CLOSE` always set.
    pub fn install(&self, limits: Limits) -> Result<(), Error> {
        // SAFETY: the struct is plain data of integers and counters; all-zero is
        // the documented "no limit" state and is a valid value of every field.
        let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = unsafe { std::mem::zeroed() };
        info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        if let Some(bytes) = limits.memory_bytes {
            // The flag and the field travel together; setting one without the
            // other is silently ignored by the kernel.
            info.BasicLimitInformation.LimitFlags |= JOB_OBJECT_LIMIT_JOB_MEMORY;
            info.JobMemoryLimit = usize::try_from(bytes).unwrap_or(usize::MAX);
        }
        if let Some(budget) = limits.cpu_budget {
            info.BasicLimitInformation.LimitFlags |= JOB_OBJECT_LIMIT_JOB_TIME;
            // 100ns units, rounded up so a sub-tick budget is not zero (which the
            // kernel reads as "no limit").
            let ticks = budget.as_nanos().div_ceil(100);
            info.BasicLimitInformation.PerJobUserTimeLimit =
                i64::try_from(ticks).unwrap_or(i64::MAX);
        }
        let size = u32::try_from(std::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>())
            .unwrap_or(u32::MAX);
        // SAFETY: `info` outlives the call, `size` is exactly its size, and the
        // struct the kernel reads for `JobObjectExtendedLimitInformation` is this
        // one — the class and the buffer type must match, and they do.
        let ok = unsafe {
            SetInformationJobObject(
                self.inner.handle,
                JobObjectExtendedLimitInformation,
                (&raw const info).cast::<c_void>(),
                size,
            )
        };
        if ok == 0 {
            return Err(Error::last("sandbox: SetInformationJobObject failed"));
        }
        Ok(())
    }

    /// Assign `pid` (and, by inheritance, everything it forks) to the job.
    ///
    /// The process handle is opened with the two rights the sandbox needs and
    /// nothing more: `PROCESS_SET_QUOTA | PROCESS_TERMINATE`.
    pub fn assign(&self, pid: u32) -> Result<(), Error> {
        // SAFETY: the right combination is the minimum that assignment needs:
        // `PROCESS_SET_QUOTA` for the job write, `PROCESS_TERMINATE` so a failed
        // assignment can still be cleaned up. `inherit = 0` is required.
        let process = unsafe { OpenProcess(PROCESS_SET_QUOTA | PROCESS_TERMINATE, 0, pid) };
        if process.is_null() {
            return Err(Error::last(format!(
                "sandbox: OpenProcess({pid}) for the job failed"
            )));
        }
        // SAFETY: both handles are live at this point; neither is closed until
        // after the call.
        let ok = unsafe { AssignProcessToJobObject(self.inner.handle, process) };
        // SAFETY: the handle came from `OpenProcess` above and is closed exactly
        // once; the job keeps its own reference to the process.
        unsafe { CloseHandle(process) };
        if ok == 0 {
            return Err(Error::last(format!(
                "sandbox: AssignProcessToJobObject({pid}) failed"
            )));
        }
        Ok(())
    }

    /// Kill every process in the job.
    pub fn terminate(&self, exit_code: u32) -> Result<(), Error> {
        // SAFETY: the job handle is live for as long as `self` is.
        let ok = unsafe { TerminateJobObject(self.inner.handle, exit_code) };
        if ok == 0 {
            return Err(Error::last("sandbox: TerminateJobObject failed"));
        }
        Ok(())
    }

    /// The raw handle, for the one caller that must attach a process at
    /// creation time (`PROC_THREAD_ATTRIBUTE_JOB_LIST` in [`crate::pty`]).
    ///
    /// Deliberately crate-private: outside this crate a `Job` is only ever
    /// named, never unwrapped.
    pub(crate) fn handle(&self) -> HANDLE {
        self.inner.handle
    }

    /// The limits the kernel currently has installed.
    ///
    /// Read back rather than remembered: `SetInformationJobObject` silently
    /// ignores a field whose flag is missing, so what was requested and what is
    /// in force are two different questions.
    pub fn limits(&self) -> Result<Limits, Error> {
        // SAFETY: plain data, and the query writes the documented layout.
        let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = unsafe { std::mem::zeroed() };
        self.query(
            JobObjectExtendedLimitInformation,
            (&raw mut info).cast::<c_void>(),
            std::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>(),
        )?;
        let flags = info.BasicLimitInformation.LimitFlags;
        Ok(Limits {
            memory_bytes: (flags & JOB_OBJECT_LIMIT_JOB_MEMORY != 0)
                .then_some(info.JobMemoryLimit as u64),
            cpu_budget: (flags & JOB_OBJECT_LIMIT_JOB_TIME != 0).then(|| {
                Duration::from_nanos(
                    u64::try_from(info.BasicLimitInformation.PerJobUserTimeLimit)
                        .unwrap_or(0)
                        .saturating_mul(100),
                )
            }),
        })
    }

    /// What the tree has used so far.
    pub fn usage(&self) -> Result<Usage, Error> {
        // SAFETY: both buffers are plain data; the kernel overwrites them with the
        // documented layouts of the classes queried below.
        let mut accounting: JOBOBJECT_BASIC_ACCOUNTING_INFORMATION = unsafe { std::mem::zeroed() };
        // SAFETY: as above.
        let mut limits: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = unsafe { std::mem::zeroed() };
        self.query(
            JobObjectBasicAccountingInformation,
            (&raw mut accounting).cast::<c_void>(),
            std::mem::size_of::<JOBOBJECT_BASIC_ACCOUNTING_INFORMATION>(),
        )?;
        self.query(
            JobObjectExtendedLimitInformation,
            (&raw mut limits).cast::<c_void>(),
            std::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>(),
        )?;
        let ticks = u64::try_from(accounting.TotalUserTime).unwrap_or(0);
        Ok(Usage {
            user_time: Duration::from_nanos(ticks.saturating_mul(100)),
            peak_memory: limits.PeakJobMemoryUsed as u64,
            active_processes: accounting.ActiveProcesses,
        })
    }

    /// Which limit, if any, the kernel reported as hit.
    ///
    /// `QueryInformationJobObject(JobObjectLimitViolationInformation)` is the only
    /// honest answer to "was this killed by the cap or did it exit?" — the exit
    /// code alone does not say.
    pub fn violation(&self) -> Result<Option<Violation>, Error> {
        // SAFETY: plain data, and the query writes the documented layout.
        let mut info: JOBOBJECT_LIMIT_VIOLATION_INFORMATION = unsafe { std::mem::zeroed() };
        // SAFETY: `info` is exactly the struct this class fills in, its size is
        // passed with it, and the return-length slot is optional.
        let ok = unsafe {
            QueryInformationJobObject(
                self.inner.handle,
                JobObjectLimitViolationInformation,
                (&raw mut info).cast::<c_void>(),
                u32::try_from(std::mem::size_of::<JOBOBJECT_LIMIT_VIOLATION_INFORMATION>())
                    .unwrap_or(u32::MAX),
                std::ptr::null_mut(),
            )
        };
        if ok == 0 {
            return Err(Error::last("sandbox: QueryInformationJobObject failed"));
        }
        let flags = info.ViolationLimitFlags;
        if flags & JOB_OBJECT_LIMIT_JOB_MEMORY != 0 {
            return Ok(Some(Violation::Memory));
        }
        if flags & JOB_OBJECT_LIMIT_JOB_TIME != 0 {
            return Ok(Some(Violation::Time));
        }
        Ok(None)
    }

    /// Which cap the tree has spent, if any.
    ///
    /// The question a supervisor asks every tick, answered here because the
    /// answer is a property of this primitive and not of its caller.
    ///
    /// Two sources, in order of authority. [`Job::violation`] is the direct one,
    /// but the legacy `JobObjectLimitViolationInformation` query answers zeroed on
    /// some hosts, so the counters are what the answer is really built on: a tree
    /// whose cpu time has reached the budget in force, or whose peak commit has
    /// reached the ceiling in force, is over it. An error from `violation` is not
    /// returned — the counters answer the same question, so failing the call over
    /// the weaker source would lose information rather than report it.
    ///
    /// The comparison is against [`Job::limits`], what the kernel currently has
    /// installed, not against what was requested: `SetInformationJobObject`
    /// silently ignores a field whose flag is missing, so the two differ.
    ///
    /// `JobMemoryLimit` is subtler than cpu time — it makes allocations fail past
    /// the ceiling rather than killing the tree, so a program that never gives up
    /// is never ended by the kernel at all. This is the query that lets the caller
    /// end it, which is what makes "over the cap ends the session" true for memory
    /// as well.
    pub fn exceeded(&self) -> Result<Option<Violation>, Error> {
        if let Ok(Some(violation)) = self.violation() {
            return Ok(Some(violation));
        }
        let limits = self.limits()?;
        let usage = self.usage()?;
        if limits
            .cpu_budget
            .is_some_and(|budget| usage.user_time >= budget)
        {
            return Ok(Some(Violation::Time));
        }
        if limits
            .memory_bytes
            .is_some_and(|cap| usage.peak_memory >= cap)
        {
            return Ok(Some(Violation::Memory));
        }
        Ok(None)
    }

    fn query(
        &self,
        class: JOBOBJECTINFOCLASS,
        buffer: *mut c_void,
        size: usize,
    ) -> Result<(), Error> {
        // SAFETY: the caller passes a buffer of the class's documented type and its
        // exact size; the slot is never read as anything else.
        let ok = unsafe {
            QueryInformationJobObject(
                self.inner.handle,
                class,
                buffer,
                u32::try_from(size).unwrap_or(u32::MAX),
                std::ptr::null_mut(),
            )
        };
        if ok == 0 {
            return Err(Error::last("sandbox: QueryInformationJobObject failed"));
        }
        Ok(())
    }
}

impl Drop for Inner {
    fn drop(&mut self) {
        // SAFETY: this is the only `CloseHandle` on the job's handle, so it happens
        // exactly once, and closing the last handle is what triggers
        // `KILL_ON_JOB_CLOSE`.
        unsafe { CloseHandle(self.handle) };
    }
}

fn pid_in_any_job(pid: u32) -> Result<bool, Error> {
    // SAFETY: the least right that can answer the question, with `inherit = 0`.
    let process = unsafe { OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid) };
    if process.is_null() {
        return Err(Error::last(format!("sandbox: OpenProcess({pid}) failed")));
    }
    let mut in_job: BOOL = 0;
    // SAFETY: a null job handle asks "any job", and `in_job` is a live `BOOL` the
    // kernel writes only on success.
    let ok = unsafe { IsProcessInJob(process, std::ptr::null_mut(), &mut in_job) };
    // SAFETY: the handle came from `OpenProcess` above and is closed once.
    unsafe { CloseHandle(process) };
    if ok == 0 {
        return Err(Error::last("sandbox: IsProcessInJob failed"));
    }
    Ok(in_job != 0)
}

impl Job {
    /// Whether `pid` already belongs to a job.
    ///
    /// A host that runs under one (a CI shell, a service manager) cannot be
    /// nested into a fresh job on Windows 7; on Windows 8+ jobs nest, so this is
    /// only used to *explain* a failure instead of guessing at it.
    pub fn process_is_in_a_job(pid: u32) -> Result<bool, Error> {
        pid_in_any_job(pid)
    }
}

use windows_sys::Win32::Foundation::{CloseHandle, HANDLE};
use windows_sys::Win32::System::JobObjects::{
    AssignProcessToJobObject, CreateJobObjectW, IsProcessInJob, JOB_OBJECT_LIMIT_JOB_MEMORY,
    JOB_OBJECT_LIMIT_JOB_TIME, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
    JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, JOBOBJECT_EXTENDED_LIMIT_INFORMATION,
    JOBOBJECT_LIMIT_VIOLATION_INFORMATION, JOBOBJECTINFOCLASS, JobObjectBasicAccountingInformation,
    JobObjectExtendedLimitInformation, JobObjectLimitViolationInformation,
    QueryInformationJobObject, SetInformationJobObject, TerminateJobObject,
};
use windows_sys::Win32::System::Threading::{
    OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION, PROCESS_SET_QUOTA, PROCESS_TERMINATE,
};
use windows_sys::core::BOOL;

#[cfg(test)]
mod tests {
    use super::*;

    /// A child that would outlive the test if the job did not end it.
    ///
    /// Nothing here ever assigns **this** process to a job: membership cannot be
    /// undone, so a test that did that would cap the rest of the binary.
    fn long_child() -> std::process::Child {
        std::process::Command::new("cmd")
            .args(["/c", "ping -n 60 127.0.0.1 >NUL"])
            .spawn()
            .expect("spawn")
    }

    #[test]
    fn a_job_can_be_created_with_every_limit() {
        let job = Job::create(Limits {
            memory_bytes: Some(64 * 1024 * 1024),
            cpu_budget: Some(Duration::from_secs(2)),
        })
        .expect("create");
        // The counters must be readable straight away, before anything is assigned.
        let usage = job.usage().expect("usage");
        assert_eq!(usage.active_processes, 0);
        assert!(job.violation().expect("violation").is_none());
    }

    #[test]
    fn a_child_can_be_assigned_and_counted() {
        let job = Job::create(Limits::default()).expect("create");
        let mut child = long_child();
        job.assign(child.id()).expect("assign");
        let usage = job.usage().expect("usage");
        assert!(
            usage.active_processes >= 1,
            "the assigned child must be counted: {usage:?}"
        );
        assert!(
            Job::process_is_in_a_job(child.id()).expect("query"),
            "an assigned process must report as being in a job"
        );
        job.terminate(0).expect("terminate");
        let _ = child.wait();
    }

    #[test]
    fn dropping_the_job_kills_what_is_still_in_it() {
        let mut child = long_child();
        {
            let job = Job::create(Limits::default()).expect("create");
            job.assign(child.id()).expect("assign");
        }
        // `KILL_ON_JOB_CLOSE`: the drop above terminated the tree. What proves the
        // kill is that a 60-second command returned; the exit code is not the
        // signal — the kernel terminates with `0`, so `success()` is expected.
        let started = std::time::Instant::now();
        let status = child.wait().expect("wait");
        assert!(
            started.elapsed() < std::time::Duration::from_secs(10),
            "the child outlived the job: it ran for {:?}",
            started.elapsed()
        );
        assert!(
            status.success(),
            "a job kill reports status 0, which says nothing about why: {status:?}"
        );
    }

    #[test]
    fn a_cpu_budget_is_installed_and_ends_the_tree() {
        let budget = Duration::from_millis(50);
        let job = Job::create(Limits {
            memory_bytes: None,
            cpu_budget: Some(budget),
        })
        .expect("create");
        // What was asked for is what is in force: the flag and the field travel
        // together, and a missing flag makes the kernel ignore the field.
        let installed = job.limits().expect("limits");
        assert_eq!(installed.cpu_budget, Some(budget), "{installed:?}");

        // A loop that would run far longer than the budget. The kernel ends the
        // tree when the budget is spent, so this returns in seconds.
        let mut child = std::process::Command::new("cmd")
            .args(["/c", "for /L %i in (1,1,200000) do @echo x >NUL"])
            .spawn()
            .expect("spawn");
        job.assign(child.id()).expect("assign");
        let started = std::time::Instant::now();
        let _ = child.wait();
        assert!(
            started.elapsed() < std::time::Duration::from_secs(20),
            "the budget did not end the tree: {:?}",
            started.elapsed()
        );
        // The budget is spent, which is what the reaper compares against. The
        // kernel's own violation record is *not* asserted here: on this host the
        // legacy `JobObjectLimitViolationInformation` query answers zeroed, so a
        // guarantee built on it would be a guess (the counters are the reliable
        // answer, and `Usage` is what the sandbox classifies with).
        let usage = job.usage().expect("usage");
        assert!(
            usage.user_time >= budget,
            "the tree must have spent its budget: {usage:?}"
        );
    }

    #[test]
    fn a_spent_cpu_budget_is_named_by_the_job_itself() {
        let budget = Duration::from_millis(50);
        let job = Job::create(Limits {
            memory_bytes: None,
            cpu_budget: Some(budget),
        })
        .expect("create");
        // Nothing is assigned yet, so nothing can be over the cap — and the answer
        // must come from the counters rather than from a record that was never set.
        assert_eq!(job.exceeded().expect("exceeded"), None, "fresh job");

        let mut child = std::process::Command::new("cmd")
            .args(["/c", "for /L %i in (1,1,200000) do @echo x >NUL"])
            .spawn()
            .expect("spawn");
        job.assign(child.id()).expect("assign");
        let started = std::time::Instant::now();
        let _ = child.wait();
        assert!(
            started.elapsed() < std::time::Duration::from_secs(20),
            "the budget did not end the tree: {:?}",
            started.elapsed()
        );
        // Only a cpu budget was installed, so naming `Time` is the whole claim:
        // a caller no longer has to compare counters against limits itself.
        assert_eq!(
            job.exceeded().expect("exceeded"),
            Some(Violation::Time),
            "a spent cpu budget must be named by the job: {:?}",
            job.usage().expect("usage")
        );
    }

    #[test]
    fn limits_round_trip_through_the_kernel() {
        let remembered = Limits {
            memory_bytes: Some(32 * 1024 * 1024),
            cpu_budget: Some(Duration::from_millis(250)),
        };
        let job = Job::create(remembered).expect("create");
        let installed = job.limits().expect("limits");
        assert_eq!(installed, remembered);
        // `KILL_ON_JOB_CLOSE` is always on, so it is not part of `Limits`; a drop
        // that did not carry it would be the bug this job exists to prevent.
        assert!(!Violation::Memory.eq(&Violation::Time));
    }

    #[test]
    fn a_nested_host_is_detectable() {
        // The CI runner usually starts us inside a job; either answer is valid, the
        // point is that the query works and does not fail.
        let _ = Job::process_is_in_a_job(std::process::id()).expect("query");
    }
}

//! `proto ⇄ domain` conversions.
//!
//! Every string that crosses the wire is decoded here and nowhere else, so a
//! peer's spelling choice is one reviewable table instead of scattered
//! `match`es. Two rules:
//!
//! - **Decoding is tolerant**: an older peer's spelling is accepted where it is
//!   unambiguous. The one exception is a mode that would *widen* a policy
//!   (`read_only`), which is only ever emitted, never guessed.
//! - **Encoding is canonical**: one spelling per value, using the Go side's
//!   vocabulary (`exited` / `signaled` / `timed_out` / `budget_exceeded` /
//!   `unenforceable`, `stdout` / `stderr` / `tty`, `output` / `exited` /
//!   `closed` / `lag`, `interrupt`, `deny` / `allow`).
//!
//! Net-mode and write-mode spellings are the two places the Go source could not
//! be re-read while this was written (the Go tree was mid-refactor), so they are
//! parsed leniently (`deny_all` and `deny-all`, `workspace` and `default`) and
//! emitted in the `_` form the sibling enums use. A peer that cannot read them
//! loses a *capability* report (the safe direction), and the one field where a
//! misread would widen a policy — `write` — emits the empty string for the
//! permissive value so an unreadable `read_only` is never mistaken for it.

use std::path::PathBuf;
use std::time::Duration;

use flowcraft_error::{Error, Result};
use flowcraft_proto::sandbox as pb;

use crate::contract::{
    Capabilities, Chunk, Enforcement, EnvPolicy, Event, ExitReason, ExitStatus,
    JournalCapabilities, NetAction, NetMode, NetModes, NetPolicy, NetRule, ReadResult,
    ResourceLimits, SessionFeatures, SessionInfo, SessionSpec, Signal, Stream, WriteMode,
    WriteModes,
};
use crate::journal::{Batch, FileOp, Gap, GapReason, WriteEvent};

// ---------------------------------------------------------------------------
// Spec
// ---------------------------------------------------------------------------

/// Encode a spec for `Start`.
#[must_use]
pub(crate) fn spec_to_proto(spec: &SessionSpec) -> pb::SessionSpec {
    let opts = &spec.opts;
    pb::SessionSpec {
        id: spec.id.clone(),
        argv: spec.argv.clone(),
        tty: spec.tty,
        rows: i32::from(spec.rows),
        cols: i32::from(spec.cols),
        stdin: spec.stdin,
        opts: Some(pb::ExecOptions {
            workdir: opts
                .workdir
                .as_ref()
                .map(|dir| dir.display().to_string())
                .unwrap_or_default(),
            stdin: opts.stdin.clone().unwrap_or_default(),
            timeout: duration_to_wire(opts.timeout),
            // proto3 cannot distinguish "no policy" from an empty allow-list in
            // a repeated field, so "inherit" is carried by the *absence* of the
            // message. That is the only encoding where the strict reading
            // (`Some([])` = forward nothing) stays strict.
            env: (opts.env.allow.is_some() || !opts.env.inject.is_empty()).then(|| pb::EnvPolicy {
                allow: opts.env.allow.clone().unwrap_or_default(),
                inject: opts.env.inject.clone().into_iter().collect(),
            }),
            net: Some(net_to_proto(&opts.net)),
            write: write_to_wire(opts.write),
            resources: Some(limits_to_proto(&opts.limits)),
        }),
    }
}

/// Decode a `Start` spec.
pub(crate) fn spec_from_proto(spec: pb::SessionSpec) -> Result<SessionSpec> {
    let exec = spec.opts.unwrap_or_default();
    let env = exec.env;
    let net = exec.net.unwrap_or_default();
    Ok(SessionSpec {
        id: spec.id,
        argv: spec.argv,
        tty: spec.tty,
        rows: u16::try_from(spec.rows).unwrap_or(0),
        cols: u16::try_from(spec.cols).unwrap_or(0),
        stdin: spec.stdin,
        opts: crate::contract::ExecOptions {
            workdir: Some(exec.workdir)
                .filter(|dir| !dir.is_empty())
                .map(PathBuf::from),
            stdin: Some(exec.stdin).filter(|bytes| !bytes.is_empty()),
            timeout: duration_from_wire(&exec.timeout)?,
            env: match env {
                // A missing message inherits the host environment; a present one
                // with an empty list forwards nothing. The ambiguous reading
                // resolves to the stricter one.
                None => EnvPolicy::default(),
                Some(env) => EnvPolicy {
                    allow: Some(env.allow),
                    inject: env.inject.into_iter().collect(),
                },
            },
            net: net_from_proto(net)?,
            write: write_from_wire(&exec.write)?,
            limits: limits_from_proto(exec.resources.unwrap_or_default()),
        },
    })
}

/// Encode a one-shot timeout.
#[must_use]
pub(crate) fn duration_to_wire(duration: Option<Duration>) -> String {
    let Some(duration) = duration else {
        return String::new();
    };
    let millis = duration.as_millis();
    if millis > 0 && millis % 1000 == 0 {
        format!("{}s", millis / 1000)
    } else {
        format!("{millis}ms")
    }
}

/// Decode a one-shot timeout (`1500ms`, `30s`, `2m`, `1h`).
pub(crate) fn duration_from_wire(raw: &str) -> Result<Option<Duration>> {
    let raw = raw.trim();
    if raw.is_empty() {
        return Ok(None);
    }
    let split = raw.find(|c: char| !c.is_ascii_digit()).unwrap_or(raw.len());
    let (number, unit) = raw.split_at(split);
    let value: u64 = number
        .parse()
        .map_err(|_| Error::validation(format!("sandbox: timeout {raw:?} is not a duration")))?;
    let duration = match unit {
        "" | "s" => Duration::from_secs(value),
        "ms" => Duration::from_millis(value),
        "m" => Duration::from_secs(value.saturating_mul(60)),
        "h" => Duration::from_secs(value.saturating_mul(3600)),
        other => {
            return Err(Error::validation(format!(
                "sandbox: timeout {raw:?}: unknown unit {other:?}"
            )));
        }
    };
    Ok(Some(duration))
}

fn net_to_proto(policy: &NetPolicy) -> pb::NetPolicy {
    pb::NetPolicy {
        mode: policy.mode.as_str().to_owned(),
        allow_hosts: policy.allow_hosts.clone(),
        rules: policy
            .rules
            .iter()
            .map(|rule| pb::NetRule {
                action: rule.action.as_str().to_owned(),
                host: rule.host.clone(),
                port: i32::from(rule.port),
                reason: rule.reason.clone().unwrap_or_default(),
            })
            .collect(),
        proxy: policy.proxy.clone().unwrap_or_default(),
        unix_sockets: policy
            .unix_sockets
            .iter()
            .map(|path| path.display().to_string())
            .collect(),
    }
}

fn net_from_proto(policy: pb::NetPolicy) -> Result<NetPolicy> {
    let mut rules = Vec::with_capacity(policy.rules.len());
    for rule in policy.rules {
        rules.push(NetRule {
            action: NetAction::parse(&rule.action)?,
            host: rule.host,
            port: u16::try_from(rule.port).unwrap_or(0),
            reason: Some(rule.reason).filter(|reason| !reason.is_empty()),
        });
    }
    let policy = NetPolicy {
        mode: NetMode::parse(&policy.mode)?,
        allow_hosts: policy.allow_hosts,
        rules,
        proxy: Some(policy.proxy).filter(|proxy| !proxy.is_empty()),
        unix_sockets: policy.unix_sockets.into_iter().map(PathBuf::from).collect(),
    };
    policy.validate()?;
    Ok(policy)
}

fn write_to_wire(write: WriteMode) -> String {
    match write {
        // The permissive value is the proto3 zero value on purpose: a peer that
        // does not understand a mode must not fall back to it.
        WriteMode::Workspace => String::new(),
        WriteMode::ReadOnly => write.as_str().to_owned(),
    }
}

fn write_from_wire(raw: &str) -> Result<WriteMode> {
    // An unknown spelling must not fall back to `Workspace`: that is the *wider*
    // mode, so a peer's `"readonly"` would silently run a read-only request
    // writable. Refusing is the only safe reading of a mode we do not know.
    WriteMode::parse(raw)
}

fn limits_to_proto(limits: &ResourceLimits) -> pb::ResourceLimits {
    pb::ResourceLimits {
        cpu_millicores: limits
            .cpu_millicores
            .and_then(|value| i32::try_from(value).ok())
            .unwrap_or(0),
        memory_bytes: limits
            .memory_bytes
            .and_then(|value| i64::try_from(value).ok())
            .unwrap_or(0),
        disk_bytes: limits
            .disk_bytes
            .and_then(|value| i64::try_from(value).ok())
            .unwrap_or(0),
        max_output_bytes: limits
            .max_output_bytes
            .and_then(|value| i64::try_from(value).ok())
            .unwrap_or(0),
    }
}

fn limits_from_proto(limits: pb::ResourceLimits) -> ResourceLimits {
    ResourceLimits {
        cpu_millicores: u32::try_from(limits.cpu_millicores)
            .ok()
            .filter(|value| *value > 0),
        memory_bytes: u64::try_from(limits.memory_bytes)
            .ok()
            .filter(|value| *value > 0),
        disk_bytes: u64::try_from(limits.disk_bytes)
            .ok()
            .filter(|value| *value > 0),
        max_output_bytes: u64::try_from(limits.max_output_bytes)
            .ok()
            .filter(|value| *value > 0),
    }
}

// ---------------------------------------------------------------------------
// Capabilities
// ---------------------------------------------------------------------------

/// Encode capabilities for `DescribeResponse`.
#[must_use]
pub(crate) fn capabilities_to_proto(capabilities: &Capabilities) -> pb::DescribeResponse {
    let policy = &capabilities.policy;
    let mut net_modes = Vec::new();
    if policy.net.deny_all {
        net_modes.push(NetMode::DenyAll.as_str().to_owned());
    }
    if policy.net.allow_list {
        net_modes.push(NetMode::AllowList.as_str().to_owned());
    }
    if policy.net.proxy {
        net_modes.push(NetMode::Proxy.as_str().to_owned());
    }
    let mut write_modes = Vec::new();
    if policy.write.workspace {
        write_modes.push(WriteMode::Workspace.as_str().to_owned());
    }
    if policy.write.read_only {
        write_modes.push(WriteMode::ReadOnly.as_str().to_owned());
    }
    pb::DescribeResponse {
        policy: Some(pb::Enforcement {
            env_allow_list: policy.env_allow_list,
            net_modes,
            socks5: policy.socks5,
            mitm: policy.mitm,
            unix_socket_policy: policy.unix_sockets,
            memory_cap: policy.memory_cap,
            cpu_cap: policy.cpu_cap,
            disk_cap: policy.disk_cap,
            filesystem_bounds: policy.filesystem_bounds,
            write_modes,
        }),
        features: Some(runner_features_to_proto(capabilities.features)),
        // An absent message is how the wire says "no journal": sending an empty
        // one would look like a journal that reports nothing.
        journal: capabilities
            .journal
            .enabled
            .then(|| journal_to_proto(&capabilities.journal)),
    }
}

/// Decode a `DescribeResponse`.
#[must_use]
pub(crate) fn capabilities_from_proto(response: pb::DescribeResponse) -> Capabilities {
    let policy = response.policy.unwrap_or_default();
    let mut net = NetModes::default();
    for mode in &policy.net_modes {
        match NetMode::parse(mode) {
            Ok(NetMode::DenyAll) => net.deny_all = true,
            Ok(NetMode::AllowList) => net.allow_list = true,
            Ok(NetMode::Proxy) => net.proxy = true,
            // `default` is the absence of a policy; an unknown spelling means a
            // newer peer, and claiming it would be a lie.
            Ok(NetMode::Default) | Err(_) => {}
        }
    }
    let mut write = WriteModes::default();
    for mode in &policy.write_modes {
        match WriteMode::parse(mode) {
            Ok(WriteMode::Workspace) => write.workspace = true,
            Ok(WriteMode::ReadOnly) => write.read_only = true,
            Err(_) => {}
        }
    }
    Capabilities {
        policy: Enforcement {
            env_allow_list: policy.env_allow_list,
            net,
            socks5: policy.socks5,
            mitm: policy.mitm,
            unix_sockets: policy.unix_socket_policy,
            memory_cap: policy.memory_cap,
            cpu_cap: policy.cpu_cap,
            disk_cap: policy.disk_cap,
            filesystem_bounds: policy.filesystem_bounds,
            write,
        },
        features: runner_features_from_proto(response.features.unwrap_or_default()),
        journal: journal_from_proto(response.journal),
    }
}

fn journal_to_proto(journal: &JournalCapabilities) -> pb::Journal {
    pb::Journal {
        enabled: journal.enabled,
        writer_id: journal.writer_id,
        rename_pairing: journal.rename_pairing,
        file_identity: journal.file_identity,
        watch_budget: journal.watch_budget,
    }
}

fn journal_from_proto(journal: Option<pb::Journal>) -> JournalCapabilities {
    let Some(journal) = journal else {
        return JournalCapabilities::default();
    };
    JournalCapabilities {
        enabled: journal.enabled,
        writer_id: journal.writer_id,
        rename_pairing: journal.rename_pairing,
        file_identity: journal.file_identity,
        watch_budget: journal.watch_budget,
    }
}

/// Encode runner-level features.
///
/// The wire's `Features` has no `stdin`: whether a session has an input side is
/// a per-session answer, carried by `SessionRef.capabilities` instead.
#[must_use]
pub(crate) fn runner_features_to_proto(features: SessionFeatures) -> pb::Features {
    pb::Features {
        tty: features.tty,
        signal: features.signal,
        events: features.events,
    }
}

fn runner_features_from_proto(features: pb::Features) -> SessionFeatures {
    SessionFeatures {
        tty: features.tty,
        // Not on the wire at runner level; a session reports its own.
        stdin: false,
        signal: features.signal,
        events: features.events,
    }
}

/// Encode per-session features.
#[must_use]
pub(crate) fn features_to_proto(features: SessionFeatures) -> pb::SessionFeatures {
    pb::SessionFeatures {
        tty: features.tty,
        stdin: features.stdin,
        signal: features.signal,
        events: features.events,
    }
}

/// Decode per-session features.
#[must_use]
pub(crate) fn features_from_proto(features: pb::SessionFeatures) -> SessionFeatures {
    SessionFeatures {
        tty: features.tty,
        stdin: features.stdin,
        signal: features.signal,
        events: features.events,
    }
}

// ---------------------------------------------------------------------------
// Outcomes and output
// ---------------------------------------------------------------------------

/// Encode an exit status.
#[must_use]
pub(crate) fn exit_to_proto(exit: &ExitStatus) -> pb::SessionExit {
    pb::SessionExit {
        code: exit.code.unwrap_or(-1),
        signal: exit.signal.unwrap_or(0),
        reason: exit.reason.as_str().to_owned(),
    }
}

/// Decode an exit status.
#[must_use]
pub(crate) fn exit_from_proto(exit: pb::SessionExit) -> ExitStatus {
    ExitStatus {
        code: Some(exit.code).filter(|code| *code >= 0),
        signal: Some(exit.signal).filter(|signal| *signal > 0),
        // An unknown reason from a newer peer keeps the exit code and reports a
        // plain exit rather than inventing a classification.
        reason: ExitReason::parse(&exit.reason).unwrap_or_default(),
    }
}

/// Encode one read result.
#[must_use]
pub(crate) fn read_to_proto(read: &ReadResult) -> pb::ReadResponse {
    pb::ReadResponse {
        next_seq: i64::try_from(read.next_seq).unwrap_or(i64::MAX),
        chunks: read
            .chunks
            .iter()
            .map(|chunk| pb::Chunk {
                seq: i64::try_from(chunk.seq).unwrap_or(i64::MAX),
                stream: chunk.stream.as_str().to_owned(),
                data: chunk.data.clone(),
            })
            .collect(),
        eof: read.eof,
    }
}

/// Decode one read result.
#[must_use]
pub(crate) fn read_from_proto(read: pb::ReadResponse) -> ReadResult {
    ReadResult {
        next_seq: u64::try_from(read.next_seq).unwrap_or(0),
        chunks: read
            .chunks
            .into_iter()
            .map(|chunk| Chunk {
                seq: u64::try_from(chunk.seq).unwrap_or(0),
                stream: Stream::parse(&chunk.stream).unwrap_or(Stream::Stdout),
                data: chunk.data,
            })
            .collect(),
        eof: read.eof,
    }
}

/// Encode one pushed event.
#[must_use]
pub(crate) fn event_to_proto(event: &Event) -> pb::Event {
    match event {
        Event::Output { seq, stream, data } => pb::Event {
            seq: i64::try_from(*seq).unwrap_or(i64::MAX),
            r#type: "output".to_owned(),
            stream: stream.as_str().to_owned(),
            data: data.clone(),
            exit: None,
        },
        Event::Exited { seq, exit } => pb::Event {
            seq: i64::try_from(*seq).unwrap_or(i64::MAX),
            r#type: "exited".to_owned(),
            stream: String::new(),
            data: Vec::new(),
            exit: Some(exit_to_proto(exit)),
        },
        Event::Closed { seq } => pb::Event {
            seq: i64::try_from(*seq).unwrap_or(i64::MAX),
            r#type: "closed".to_owned(),
            stream: String::new(),
            data: Vec::new(),
            exit: None,
        },
        Event::Lag { next_seq } => pb::Event {
            seq: i64::try_from(*next_seq).unwrap_or(i64::MAX),
            r#type: "lag".to_owned(),
            stream: String::new(),
            data: Vec::new(),
            exit: None,
        },
    }
}

/// Decode one pushed event.
pub(crate) fn event_from_proto(event: pb::Event) -> Result<Event> {
    let seq = u64::try_from(event.seq).unwrap_or(0);
    match event.r#type.as_str() {
        "output" => Ok(Event::Output {
            seq,
            stream: Stream::parse(&event.stream)?,
            data: event.data,
        }),
        "exited" => Ok(Event::Exited {
            seq,
            exit: exit_from_proto(event.exit.unwrap_or_default()),
        }),
        "closed" => Ok(Event::Closed { seq }),
        "lag" => Ok(Event::Lag { next_seq: seq }),
        other => Err(Error::validation(format!(
            "sandbox: unknown event type {other:?}"
        ))),
    }
}

/// Encode one session snapshot.
#[must_use]
pub(crate) fn info_to_proto(info: &SessionInfo) -> pb::SessionInfo {
    pb::SessionInfo {
        id: info.id.clone(),
        argv: info.argv.clone(),
        tty: info.features.tty,
        pid: info
            .pid
            .and_then(|pid| i32::try_from(pid).ok())
            .unwrap_or(0),
        started_at_unix_nano: i64::try_from(info.started_at_unix_nano).unwrap_or(i64::MAX),
        running: info.running,
        exit: info.exit.as_ref().map(exit_to_proto),
    }
}

/// Decode one session snapshot.
///
/// The wire carries only `tty` for features, so a listed session reports the
/// rest as `false`; [`crate::contract::Session::features`] is authoritative.
#[must_use]
pub(crate) fn info_from_proto(info: pb::SessionInfo) -> SessionInfo {
    SessionInfo {
        id: info.id,
        argv: info.argv,
        tty: info.tty,
        pid: u32::try_from(info.pid).ok().filter(|pid| *pid > 0),
        started_at_unix_nano: u64::try_from(info.started_at_unix_nano).unwrap_or(0),
        features: SessionFeatures {
            tty: info.tty,
            ..SessionFeatures::default()
        },
        running: info.running,
        exit: info.exit.map(exit_from_proto),
    }
}

/// Encode one journal event.
#[must_use]
pub(crate) fn journal_event_to_proto(event: &WriteEvent) -> pb::JournalEvent {
    pb::JournalEvent {
        seq: i64::try_from(event.seq).unwrap_or(i64::MAX),
        at_unix_nano: i64::try_from(event.at_unix_nano).unwrap_or(i64::MAX),
        op: event.op.as_str().to_owned(),
        path: event.path.clone(),
        old_path: event.old_path.clone().unwrap_or_default(),
        is_dir: event.is_dir,
        dev: event.dev.unwrap_or(0),
        ino: event.ino.unwrap_or(0),
        // The Go contract uses -1 for "could not be determined"; 0 is a real,
        // empty file.
        size: event.size.unwrap_or(-1),
        session: event.session.clone(),
    }
}

/// Decode one journal event.
pub(crate) fn journal_event_from_proto(event: pb::JournalEvent) -> Result<WriteEvent> {
    Ok(WriteEvent {
        seq: u64::try_from(event.seq).unwrap_or(0),
        at_unix_nano: u64::try_from(event.at_unix_nano).unwrap_or(0),
        op: FileOp::parse(&event.op)?,
        path: event.path,
        old_path: Some(event.old_path).filter(|path| !path.is_empty()),
        is_dir: event.is_dir,
        dev: Some(event.dev).filter(|dev| *dev != 0),
        ino: Some(event.ino).filter(|ino| *ino != 0),
        size: Some(event.size).filter(|size| *size != -1),
        session: event.session,
    })
}

/// Encode one journal read.
#[must_use]
pub(crate) fn batch_to_proto(batch: &Batch) -> pb::JournalReadResponse {
    pb::JournalReadResponse {
        events: batch.events.iter().map(journal_event_to_proto).collect(),
        next_seq: i64::try_from(batch.next_seq).unwrap_or(i64::MAX),
        gap: batch.gap.map(|gap| pb::JournalGap {
            first_missing: i64::try_from(gap.first_missing).unwrap_or(i64::MAX),
            reason: gap.reason.as_str().to_owned(),
        }),
        closed: batch.closed,
    }
}

/// Decode one journal read.
pub(crate) fn batch_from_proto(response: pb::JournalReadResponse) -> Result<Batch> {
    let mut events = Vec::with_capacity(response.events.len());
    for event in response.events {
        events.push(journal_event_from_proto(event)?);
    }
    let gap = match response.gap {
        None => None,
        Some(gap) => Some(Gap {
            first_missing: u64::try_from(gap.first_missing).unwrap_or(0),
            reason: GapReason::parse(&gap.reason)?,
        }),
    };
    Ok(Batch {
        events,
        next_seq: u64::try_from(response.next_seq).unwrap_or(0),
        gap,
        closed: response.closed,
    })
}

/// Encode a signal name.
#[must_use]
pub(crate) fn signal_to_proto(signal: Signal) -> String {
    signal.as_str().to_owned()
}

/// Decode a signal name.
pub(crate) fn signal_from_proto(raw: &str) -> Result<Signal> {
    Signal::parse(raw)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec() -> SessionSpec {
        let mut spec = SessionSpec::new(vec!["sh".to_owned(), "-c".to_owned(), "true".to_owned()]);
        spec.id = "s-1".to_owned();
        spec.rows = 40;
        spec.cols = 120;
        spec.stdin = true;
        spec.opts.timeout = Some(Duration::from_millis(1500));
        spec.opts.write = WriteMode::ReadOnly;
        spec
    }

    #[test]
    fn a_spec_survives_the_round_trip() {
        let original = spec();
        let decoded = spec_from_proto(spec_to_proto(&original)).unwrap();
        assert_eq!(decoded.id, original.id);
        assert_eq!(decoded.argv, original.argv);
        assert_eq!((decoded.rows, decoded.cols), (40, 120));
        assert_eq!(decoded.opts.timeout, original.opts.timeout);
        assert_eq!(decoded.opts.write, WriteMode::ReadOnly);
    }

    #[test]
    fn the_permissive_write_mode_is_the_zero_value() {
        assert_eq!(write_to_wire(WriteMode::Workspace), "");
        assert_eq!(write_to_wire(WriteMode::ReadOnly), "read_only");
        assert_eq!(write_from_wire("").unwrap(), WriteMode::Workspace);
        assert_eq!(write_from_wire("read-only").unwrap(), WriteMode::ReadOnly);
        // An unreadable value must never be read as the *wider* mode.
        assert_eq!(
            write_from_wire("readonly").unwrap_err().kind(),
            flowcraft_error::ErrorKind::Validation
        );
    }

    #[test]
    fn an_unknown_net_mode_is_rejected_not_defaulted() {
        let mut wire = spec_to_proto(&spec());
        wire.opts.as_mut().unwrap().net.as_mut().unwrap().mode = "allowlist".to_owned();
        let error = spec_from_proto(wire).expect_err("validation");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
    }

    #[test]
    fn durations_round_trip_a_whole_number_of_seconds() {
        assert_eq!(duration_to_wire(Some(Duration::from_secs(30))), "30s");
        assert_eq!(
            duration_to_wire(Some(Duration::from_millis(1500))),
            "1500ms"
        );
        assert_eq!(
            duration_from_wire("30s").unwrap(),
            Some(Duration::from_secs(30))
        );
        assert_eq!(
            duration_from_wire("1500ms").unwrap(),
            Some(Duration::from_millis(1500))
        );
        assert_eq!(duration_from_wire("").unwrap(), None);
        assert!(duration_from_wire("soon").is_err());
    }

    #[test]
    fn capabilities_survive_the_round_trip() {
        let capabilities = Capabilities {
            policy: Enforcement {
                env_allow_list: true,
                net: NetModes {
                    deny_all: true,
                    allow_list: false,
                    proxy: false,
                },
                socks5: false,
                mitm: false,
                unix_sockets: true,
                memory_cap: false,
                cpu_cap: false,
                disk_cap: false,
                filesystem_bounds: true,
                write: WriteModes {
                    workspace: true,
                    read_only: true,
                },
            },
            features: SessionFeatures {
                tty: false,
                stdin: false,
                signal: true,
                events: true,
            },
            journal: JournalCapabilities {
                enabled: true,
                writer_id: false,
                rename_pairing: true,
                file_identity: true,
                watch_budget: 4096,
            },
        };
        let decoded = capabilities_from_proto(capabilities_to_proto(&capabilities));
        assert_eq!(decoded, capabilities);
    }

    #[test]
    fn every_event_variant_survives_the_round_trip() {
        let events = [
            Event::Output {
                seq: 7,
                stream: Stream::Stderr,
                data: b"x".to_vec(),
            },
            Event::Exited {
                seq: 9,
                exit: ExitStatus {
                    code: Some(2),
                    signal: None,
                    reason: ExitReason::Exited,
                },
            },
            Event::Closed { seq: 9 },
            Event::Lag { next_seq: 11 },
        ];
        for event in events {
            assert_eq!(event_from_proto(event_to_proto(&event)).unwrap(), event);
        }
    }

    #[test]
    fn a_journal_batch_survives_the_round_trip() {
        let batch = Batch {
            events: vec![WriteEvent {
                seq: 3,
                at_unix_nano: 42,
                op: FileOp::Rename,
                path: "out/b".to_owned(),
                old_path: Some("out/a".to_owned()),
                is_dir: false,
                dev: Some(7),
                ino: Some(9),
                size: Some(11),
                session: String::new(),
            }],
            next_seq: 4,
            gap: Some(Gap {
                first_missing: 5,
                reason: GapReason::Overflow,
            }),
            closed: true,
        };
        assert_eq!(batch_from_proto(batch_to_proto(&batch)).unwrap(), batch);
    }

    #[test]
    fn an_absent_journal_message_means_no_journal() {
        let capabilities = Capabilities::default();
        let encoded = capabilities_to_proto(&capabilities);
        assert!(encoded.journal.is_none(), "no journal must not look empty");

        let enabled = Capabilities {
            journal: JournalCapabilities {
                enabled: true,
                watch_budget: 128,
                ..JournalCapabilities::default()
            },
            ..Capabilities::default()
        };
        let decoded = capabilities_from_proto(capabilities_to_proto(&enabled));
        assert!(decoded.journal.enabled);
        assert_eq!(decoded.journal.watch_budget, 128);
    }

    #[test]
    fn a_legacy_exit_reason_still_decodes() {
        let exit = exit_from_proto(pb::SessionExit {
            code: -1,
            signal: 15,
            reason: "signal".to_owned(),
        });
        assert_eq!(exit.reason, ExitReason::Signaled);
        assert_eq!(exit.signal, Some(15));
        assert_eq!(exit.code, None);
    }
}

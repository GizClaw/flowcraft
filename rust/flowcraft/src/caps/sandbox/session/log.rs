//! The replayable output log shared by every local session.
//!
//! `seq` is a **byte cursor**, not a chunk counter: a chunk records the cursor
//! of its first byte and the log advances by the payload length. A caller can
//! therefore resume from any offset it has already consumed, including the
//! middle of a chunk, which is what makes a reconnecting client cheap.
//!
//! The buffer is a ring bounded by one byte budget. When it overflows the
//! oldest whole chunks are dropped and a cursor below the retained range is
//! reported as a gap — never as a silent hole. A single chunk larger than the
//! budget is always kept, so one huge line stays replayable.
//!
//! Reads block on a `watch` revision counter rather than a `Notify`: a
//! notification can be delivered between the "look" and the "await" and be
//! lost, whereas a version number that the reader has already observed is
//! guaranteed to wake it.

use std::collections::VecDeque;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{SystemTime, UNIX_EPOCH};

use crate::error::{Error, Result};
use tokio::sync::{broadcast, mpsc, watch};
use tokio_stream::wrappers::ReceiverStream;

use crate::caps::sandbox::contract::{Chunk, Event, EventStream, ExitStatus, ReadResult, Stream};

/// The largest payload one read returns, whatever the caller asked for.
const READ_BATCH_LIMIT: usize = 1 << 20;
/// How many events one watcher may fall behind before it is told to re-read.
const WATCH_QUEUE: usize = 64;
/// How many events are fanned out to live watchers.
const WATCH_FANOUT: usize = 256;

/// One buffered chunk.
#[derive(Debug)]
struct Record {
    seq: u64,
    stream: Stream,
    data: Arc<[u8]>,
}

/// The buffer itself.
#[derive(Debug)]
struct State {
    records: VecDeque<Record>,
    /// Bytes currently retained.
    total: u64,
    /// Cursor of the next byte to be appended.
    next_seq: u64,
    /// Ring budget in bytes (`0` means unbounded).
    limit: u64,
    /// Bumped on every change, so a reader can tell whether it lost a race.
    version: u64,
    exit: Option<ExitStatus>,
    closed: bool,
}

impl State {
    /// The first retained cursor: anything after it can be replayed.
    fn retained(&self) -> u64 {
        self.records
            .front()
            .map_or(self.next_seq, |record| record.seq)
    }

    fn trim(&mut self) {
        if self.limit == 0 {
            return;
        }
        // Never drop the last chunk: a payload larger than the whole budget is
        // still replayable, and dropping it would report a gap for data the
        // caller is owed.
        while self.records.len() > 1 && self.total > self.limit {
            if let Some(record) = self.records.pop_front() {
                self.total = self.total.saturating_sub(record.data.len() as u64);
            }
        }
    }

    /// Collect at most `max_bytes` starting at `after`.
    fn collect(&self, after: u64, max_bytes: usize) -> ReadResult {
        let mut remaining = max_bytes;
        let mut next_seq = after;
        let mut chunks = Vec::new();
        for record in &self.records {
            if remaining == 0 {
                break;
            }
            let end = record.seq + record.data.len() as u64;
            if next_seq >= end {
                continue;
            }
            let offset = usize::try_from(next_seq.saturating_sub(record.seq)).unwrap_or(0);
            let available = record.data.len() - offset;
            let take = available.min(remaining);
            chunks.push(Chunk {
                seq: next_seq,
                stream: record.stream,
                data: record.data[offset..offset + take].to_vec(),
            });
            next_seq += take as u64;
            remaining -= take;
        }
        ReadResult {
            next_seq,
            eof: self.exit.is_some() && next_seq >= self.next_seq,
            chunks,
        }
    }
}

/// The output log of one session.
#[derive(Debug)]
pub(crate) struct Log {
    state: Mutex<State>,
    revisions: watch::Sender<u64>,
    events: broadcast::Sender<Event>,
}

fn poisoned() -> Error {
    Error::internal("sandbox: session log lock poisoned")
}

fn closed_error() -> Error {
    Error::closed("sandbox: session is closed")
}

impl Log {
    /// A log bounded to `limit` bytes (`0` keeps everything).
    pub(crate) fn new(limit: u64) -> Self {
        let (revisions, _) = watch::channel(0);
        let (events, _) = broadcast::channel(WATCH_FANOUT);
        Self {
            state: Mutex::new(State {
                records: VecDeque::new(),
                total: 0,
                next_seq: 0,
                limit,
                version: 0,
                exit: None,
                closed: false,
            }),
            revisions,
            events,
        }
    }

    fn lock(&self) -> Result<MutexGuard<'_, State>> {
        self.state.lock().map_err(|_| poisoned())
    }

    /// Publish a state change to blocked readers and live watchers.
    fn publish(&self, state: &mut State, event: Option<Event>) {
        state.version = state.version.wrapping_add(1);
        let version = state.version;
        self.revisions.send_replace(version);
        if let Some(event) = event {
            // An error means nobody is subscribed; that is the normal case.
            let _ = self.events.send(event);
        }
    }

    /// Append one chunk.
    pub(crate) fn push(&self, stream: Stream, data: &[u8]) {
        if data.is_empty() {
            return;
        }
        let Ok(mut state) = self.lock() else {
            return;
        };
        // After the process is reaped the pump join guarantees no more data,
        // but a racing pump must not resurrect a finished log.
        if state.closed || state.exit.is_some() {
            return;
        }
        let seq = state.next_seq;
        state.records.push_back(Record {
            seq,
            stream,
            data: Arc::from(data),
        });
        state.total += data.len() as u64;
        state.next_seq += data.len() as u64;
        state.trim();
        self.publish(
            &mut state,
            Some(Event::Output {
                seq,
                stream,
                data: data.to_vec(),
            }),
        );
    }

    /// Mark the stream complete.
    pub(crate) fn finish(&self, exit: ExitStatus) {
        let Ok(mut state) = self.lock() else {
            return;
        };
        if state.exit.is_some() {
            return;
        }
        let seq = state.next_seq;
        state.exit = Some(exit.clone());
        self.publish(&mut state, Some(Event::Exited { seq, exit }));
    }

    /// Mark the log closed: readers and watchers are released.
    pub(crate) fn close(&self) {
        let Ok(mut state) = self.lock() else {
            return;
        };
        if state.closed {
            return;
        }
        let seq = state.next_seq;
        state.closed = true;
        self.publish(&mut state, Some(Event::Closed { seq }));
    }

    /// Block until output is available after `after_seq`, or the stream ends.
    pub(crate) async fn read(&self, after_seq: u64, max_bytes: usize) -> Result<ReadResult> {
        if max_bytes == 0 {
            return Err(Error::validation(
                "sandbox: read max_bytes must be positive",
            ));
        }
        let max_bytes = max_bytes.min(READ_BATCH_LIMIT);
        // Subscribe first: an append that lands between the look below and the
        // await is then visible as a moved version instead of being lost.
        let mut revisions = self.revisions.subscribe();
        loop {
            let seen = *revisions.borrow_and_update();
            let version = {
                let state = self.lock()?;
                if state.closed {
                    return Err(closed_error());
                }
                let retained = state.retained();
                if retained > after_seq {
                    return Err(Error::aborted(format!(
                        "sandbox: output sequence gap: after_seq {after_seq}, retained from {retained}"
                    )));
                }
                if state.next_seq < after_seq {
                    return Err(Error::validation(format!(
                        "sandbox: after_seq {after_seq} is beyond buffered output (next={})",
                        state.next_seq
                    )));
                }
                let collected = state.collect(after_seq, max_bytes);
                if !collected.chunks.is_empty() || collected.eof {
                    return Ok(collected);
                }
                state.version
            };
            if version != seen {
                continue;
            }
            revisions.changed().await.map_err(|_| closed_error())?;
        }
    }

    /// Subscribe to pushed events: the retained output first, then live.
    pub(crate) fn watch(&self) -> Result<EventStream> {
        let mut live = self.events.subscribe();
        let (replay, snapshot, finished) = {
            let state = self.lock()?;
            if state.closed {
                return Err(closed_error());
            }
            let mut replay = Vec::with_capacity(state.records.len());
            let mut end = state.next_seq;
            for record in &state.records {
                end = record.seq + record.data.len() as u64;
                replay.push(Event::Output {
                    seq: record.seq,
                    stream: record.stream,
                    data: record.data.to_vec(),
                });
            }
            (replay, end, state.exit.clone())
        };

        let (sender, receiver) = mpsc::channel(WATCH_QUEUE);
        tokio::spawn(async move {
            // `cursor` is the first byte this subscriber has not been handed.
            let mut cursor = snapshot;
            for event in replay {
                cursor = event_end(&event, cursor);
                if sender.send(event).await.is_err() {
                    return;
                }
            }
            // A watcher that arrives after the process ended still has to learn
            // the outcome: the exit event was broadcast before it subscribed.
            if let Some(exit) = finished {
                let _ = sender.send(Event::Exited { seq: cursor, exit }).await;
                return;
            }
            loop {
                match live.recv().await {
                    Ok(event) => {
                        // Events at or before the snapshot edge were replayed.
                        if event.seq() < snapshot {
                            continue;
                        }
                        cursor = event_end(&event, cursor);
                        if sender.send(event).await.is_err() {
                            return;
                        }
                    }
                    Err(broadcast::error::RecvError::Lagged(_)) => {
                        // The exact extent of the loss is unknown, so the
                        // reader is told where to resume reading; `read` turns
                        // an overrun into a gap of its own.
                        let _ = sender.send(Event::Lag { next_seq: cursor }).await;
                        return;
                    }
                    Err(broadcast::error::RecvError::Closed) => return,
                }
            }
        });
        Ok(Box::pin(ReceiverStream::new(receiver)))
    }
}

/// The cursor after `event`.
fn event_end(event: &Event, cursor: u64) -> u64 {
    let next = match event {
        Event::Output { seq, data, .. } => seq + data.len() as u64,
        Event::Exited { seq, .. } | Event::Closed { seq } => *seq,
        Event::Lag { next_seq } => *next_seq,
    };
    next.max(cursor)
}

/// Wall-clock nanoseconds since the epoch.
pub(crate) fn now_nanos() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |duration| duration.as_nanos() as u64)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn chunk_text(result: &ReadResult) -> String {
        result
            .chunks
            .iter()
            .map(|chunk| String::from_utf8_lossy(&chunk.data).into_owned())
            .collect()
    }

    #[tokio::test]
    async fn a_cursor_can_resume_mid_chunk() {
        let log = Log::new(0);
        log.push(Stream::Stdout, b"hello world");
        let first = log.read(0, 5).await.unwrap();
        assert_eq!(chunk_text(&first), "hello");
        assert_eq!(first.next_seq, 5);
        assert!(!first.eof);

        let second = log.read(first.next_seq, 64).await.unwrap();
        assert_eq!(chunk_text(&second), " world");
        assert_eq!(second.next_seq, 11);
    }

    #[tokio::test]
    async fn a_finished_log_reports_eof_after_the_last_byte() {
        let log = Log::new(0);
        log.push(Stream::Stdout, b"done");
        log.finish(ExitStatus::exited(0));
        // The last chunk and the EOF flag arrive together, so a client never
        // needs a second round trip just to learn the process ended.
        let read = log.read(0, 64).await.unwrap();
        assert_eq!(chunk_text(&read), "done");
        assert!(read.eof);

        let end = log.read(read.next_seq, 64).await.unwrap();
        assert!(end.chunks.is_empty());
        assert!(end.eof);
    }

    #[tokio::test]
    async fn trimming_reports_a_gap_instead_of_a_hole() {
        let log = Log::new(16);
        for index in 0..24u8 {
            log.push(Stream::Stdout, &[b'a' + index]);
        }
        // The budget keeps the newest 16 bytes, so the first eight are gone.
        let error = log.read(0, 64).await.expect_err("gap");
        assert_eq!(error.kind(), crate::error::ErrorKind::Aborted);
        assert!(error.to_string().contains("sequence gap"), "{error}");

        let read = log.read(8, 64).await.unwrap();
        assert_eq!(chunk_text(&read), "ijklmnopqrstuvwx");
    }

    #[tokio::test]
    async fn one_oversized_chunk_is_always_kept() {
        let log = Log::new(4);
        log.push(Stream::Stdout, b"0123456789");
        let read = log.read(0, 64).await.unwrap();
        assert_eq!(chunk_text(&read), "0123456789");
    }

    #[tokio::test]
    async fn a_cursor_beyond_the_end_is_a_validation_error() {
        let log = Log::new(0);
        log.push(Stream::Stdout, b"abc");
        let error = log.read(9, 64).await.expect_err("validation");
        assert_eq!(error.kind(), crate::error::ErrorKind::Validation);
    }

    #[tokio::test]
    async fn read_blocks_until_output_arrives() {
        let log = Arc::new(Log::new(0));
        let reader = {
            let log = log.clone();
            tokio::spawn(async move { log.read(0, 64).await.map(|out| chunk_text(&out)) })
        };
        tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        assert!(!reader.is_finished(), "read must not spin or return empty");
        log.push(Stream::Stdout, b"late");
        let text = reader.await.unwrap().unwrap();
        assert_eq!(text, "late");
    }

    #[tokio::test]
    async fn closing_releases_a_blocked_reader() {
        let log = Arc::new(Log::new(0));
        let reader = {
            let log = log.clone();
            tokio::spawn(async move { log.read(0, 64).await.err().map(|err| err.kind()) })
        };
        tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        log.close();
        let kind = reader.await.unwrap().expect("closed");
        assert_eq!(kind, crate::error::ErrorKind::Closed);
    }

    #[tokio::test]
    async fn watch_replays_then_streams_live() {
        let log = Log::new(0);
        log.push(Stream::Stdout, b"before");
        let mut events = log.watch().unwrap();
        use tokio_stream::StreamExt;
        let first = events.next().await.expect("replay");
        assert_eq!(
            first,
            Event::Output {
                seq: 0,
                stream: Stream::Stdout,
                data: b"before".to_vec()
            }
        );
        log.push(Stream::Stdout, b"after");
        let second = events.next().await.expect("live");
        assert_eq!(second.seq(), 6);
        log.finish(ExitStatus::exited(0));
        let third = events.next().await.expect("exited");
        assert!(matches!(third, Event::Exited { .. }));
    }
}

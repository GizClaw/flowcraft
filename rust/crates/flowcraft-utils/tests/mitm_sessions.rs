//! Replays the committed MITM contract's `sessions` and `ambient` tables against
//! the port's engine.
//!
//! Every row is one scripted tunnel, and the harness reproduces it the way the
//! dump did: the client side is an in-memory pipe under a real `rustls` client,
//! the target is a real TLS server holding a certificate from a CA of its own,
//! and the engine sits between them with the row's policy, hooks and script.
//! What comes back is compared in three places, because the row records three:
//!
//!   - `trace`, what the hooks were told, in call order;
//!   - `client`, what the client got back, framing included -- the response is
//!     parsed from bytes rather than through a client library, because the
//!     framing *is* the answer (`response-chunked` reaches the client as a
//!     length);
//!   - `upstream_saw`, what the target received: method, target, authority, SNI,
//!     protocol, headers, body, framing.
//!
//! The normalizations are the dump's, not this file's invention: header names
//! are lowercased, values keep their order, `date` and `connection` and `x-pad`
//! are masked, and a body is hex where `null` and `""` are different answers.
//! A client answer with no `framing` is the one row whose head and body belong
//! to the HTTP stack rather than to the engine, so only its status is compared.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use flowcraft_contract::error::{Error, ErrorKind};
use flowcraft_utils::net::mitm::ca::Ca;
use flowcraft_utils::net::mitm::{Engine, Hooks, RequestInfo, ResponseInfo, UpstreamDialer};
use flowcraft_utils::net::{BoxFuture, MitmPolicy, Stream};
use http_body_util::{BodyExt, Full};
use hyper::body::{Body as HttpBody, Bytes, Frame, Incoming, SizeHint};
use hyper::header::{self, HeaderMap, HeaderValue};
use hyper::service::service_fn;
use hyper::{Method, Request, Response, StatusCode};
use hyper_util::rt::{TokioExecutor, TokioIo};
use rustls::pki_types::{CertificateDer, ServerName};
use rustls::{ClientConfig, RootCertStore};
use serde_json::Value;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio_rustls::TlsConnector;
use tokio_rustls::client::TlsStream;

const CONTRACT: &str = include_str!("../../../conformance/mitm-contract.json");

/// The name every row's tunnel is opened for unless it says otherwise.
const DEFAULT_SERVER_NAME: &str = "example.test";

/// Headers a row does not compare: a clock, the writer's framing decision, and
/// the harness's own filler.
const MASKED: &[&str] = &["date", "connection", "x-pad"];

/// How long one tunnel may take before the harness calls it hung.
const SESSION_TIMEOUT: Duration = Duration::from_secs(20);

/// How much of a body the harness keeps, as the dump does.
const BODY_CAP: u64 = 1 << 21;

/// How large the in-memory pipe under the client and the engine is. It has to
/// hold the oversized-header row's megabyte even after the engine stops reading.
const PIPE: usize = 4 << 20;

#[tokio::test]
async fn the_sessions_replay() {
    let document = contract();
    let rows = rows(&document, "sessions");
    assert!(
        rows.len() >= 21,
        "sessions has {} rows, expected at least 21: a table that shrank would replay fewer claims",
        rows.len()
    );
    let mut problems = Vec::new();
    for row in rows {
        let session = Session::parse(row);
        match run(&session).await {
            Ok(observed) => session.compare(&observed, &mut problems),
            Err(err) => problems.push(format!("{}: the tunnel broke: {err}", session.name)),
        }
    }
    report(problems, "session");
}

/// `ambient` is derived from one session row, so it is checked against the row
/// the document names rather than against a second copy of the same claims.
#[tokio::test]
async fn the_ambient_section_replays() {
    let document = contract();
    let ambient = document
        .get("ambient")
        .unwrap_or_else(|| panic!("the document has no ambient section"));
    let named = string(ambient, "row");
    let mut problems = Vec::new();

    let session = rows(&document, "sessions")
        .iter()
        .find(|row| string(row, "name") == named)
        .map(Session::parse)
        .unwrap_or_else(|| panic!("ambient names {named:?} and no session row has it"));
    let observed = match run(&session).await {
        Ok(observed) => observed,
        Err(err) => panic!("{named}: the tunnel broke: {err}"),
    };

    let adds = ambient
        .get("adds_when_absent")
        .and_then(Value::as_object)
        .unwrap_or_else(|| panic!("ambient has no adds_when_absent"));
    let seen = observed
        .seen
        .first()
        .unwrap_or_else(|| panic!("{named}: the target was sent nothing"));
    for (name, value) in adds {
        let want = vec![value.as_str().unwrap_or_default().to_owned()];
        let got = seen.headers.get(name).cloned().unwrap_or_default();
        if want != got {
            problems.push(format!(
                "adds_when_absent: core sends {name}: {want:?} and the port sent {got:?}"
            ));
        }
    }
    let proto = ambient
        .get("upstream_proto")
        .and_then(Value::as_str)
        .unwrap_or_default();
    if seen.proto != proto {
        problems.push(format!(
            "upstream_proto: core's target spoke {proto:?} and the port's spoke {:?}",
            seen.proto
        ));
    }

    let decompresses = ambient
        .get("decompresses_own_gzip")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    let plaintext = observed
        .client
        .first()
        .and_then(|answer| answer.body.clone())
        .unwrap_or_default();
    let encoded = observed
        .client
        .first()
        .map(|answer| answer.headers.contains_key("content-encoding"))
        .unwrap_or(true);
    if decompresses && !encoded {
        let text = String::from_utf8_lossy(&plaintext).into_owned();
        if !text.contains("hello hello hello") {
            problems.push(format!(
                "decompresses_own_gzip: the client was handed {text:?}"
            ));
        }
    }

    report(problems, "ambient");
}

// ------------------------------------------------------------------- one row

/// One tunnel, as the document spells it.
struct Session {
    name: String,
    policy: MitmPolicy,
    negotiated: String,
    server_name: String,
    untrusted: bool,
    refuse_dial: bool,
    probes: Vec<Probe>,
    upstream: Answer,
    hooks: Option<Script>,
    trace: Vec<Event>,
    client: Vec<Answer>,
    seen: Vec<Seen>,
}

/// Which hooks the row installed and what they decided.
struct Script {
    deny_request: bool,
    deny_response: bool,
}

/// One request the client sends.
#[derive(Clone)]
struct Probe {
    method: String,
    target: String,
    authority: String,
    headers: Vec<(String, String)>,
    body: Option<Vec<u8>>,
    framing: String,
    pad_header: usize,
}

/// A response: the target's script, or the client's answer.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
struct Answer {
    status: u16,
    headers: BTreeMap<String, Vec<String>>,
    body: Option<Vec<u8>>,
    /// `None` on the client side means the row compares the status only.
    framing: Option<String>,
}

/// One hook call.
#[derive(Debug, PartialEq, Eq)]
enum Event {
    Request {
        method: String,
        scheme: String,
        host: String,
        path: String,
        headers: BTreeMap<String, Vec<String>>,
        body: Option<Vec<u8>>,
    },
    Response {
        status: u16,
        headers: BTreeMap<String, Vec<String>>,
        body: Option<Vec<u8>>,
    },
}

/// One request the target received.
#[derive(Debug, PartialEq, Eq)]
struct Seen {
    method: String,
    target: String,
    authority: String,
    sni: Option<String>,
    proto: String,
    headers: BTreeMap<String, Vec<String>>,
    body: Option<Vec<u8>>,
    framing: String,
}

/// What the row and the run are compared field by field.
struct Observed {
    trace: Vec<Event>,
    client: Vec<Answer>,
    seen: Vec<Seen>,
}

impl Session {
    fn parse(row: &Value) -> Session {
        let name = string(row, "name");
        let policy = row.get("policy").cloned().unwrap_or(Value::Null);
        let server_name = row
            .get("server_name")
            .and_then(Value::as_str)
            .unwrap_or(DEFAULT_SERVER_NAME)
            .to_owned();
        Session {
            policy: MitmPolicy {
                enabled: true,
                inspect_bodies: policy
                    .get("inspect_bodies")
                    .and_then(Value::as_bool)
                    .unwrap_or(false),
                max_body_bytes: policy
                    .get("max_body_bytes")
                    .and_then(Value::as_i64)
                    .unwrap_or(0),
                hosts: Vec::new(),
                exclude_hosts: Vec::new(),
            },
            negotiated: string(row, "negotiated"),
            server_name,
            untrusted: row
                .get("untrusted")
                .and_then(Value::as_bool)
                .unwrap_or(false),
            refuse_dial: row
                .get("refuse_dial")
                .and_then(Value::as_bool)
                .unwrap_or(false),
            probes: probes(row),
            upstream: row.get("upstream").map(answer).unwrap_or_default(),
            hooks: row.get("hooks").map(|hooks| Script {
                deny_request: hooks.get("request").and_then(Value::as_str) == Some("deny"),
                deny_response: hooks.get("response").and_then(Value::as_str) == Some("deny"),
            }),
            trace: row
                .get("trace")
                .and_then(Value::as_array)
                .map(|events| events.iter().map(event).collect())
                .unwrap_or_default(),
            client: row
                .get("client")
                .and_then(Value::as_array)
                .map(|answers| answers.iter().map(answer).collect())
                .unwrap_or_default(),
            seen: row
                .get("upstream_saw")
                .and_then(Value::as_array)
                .map(|seen| seen.iter().map(seen_row).collect())
                .unwrap_or_default(),
            name,
        }
    }

    fn compare(&self, observed: &Observed, problems: &mut Vec<String>) {
        let mut check = Check::new(&self.name, problems);
        check.value("trace", &self.trace, &observed.trace);
        check.value("client", &self.client, &observed.client);
        check.value("upstream_saw", &self.seen, &observed.seen);
    }
}

/// Collect mismatches for one row, named by the field they are in.
struct Check<'a> {
    name: &'a str,
    problems: &'a mut Vec<String>,
}

impl<'a> Check<'a> {
    fn new(name: &'a str, problems: &'a mut Vec<String>) -> Check<'a> {
        Check { name, problems }
    }

    fn value<T: PartialEq + std::fmt::Debug>(&mut self, field: &str, want: &T, got: &T) {
        if want != got {
            self.problems.push(format!(
                "{}: {field}:\n  core {want:#?}\n  port {got:#?}",
                self.name
            ));
        }
    }
}

// ------------------------------------------------------------------ the run

/// One tunnel end to end: a target, an engine, and a client that writes bytes.
async fn run(session: &Session) -> Result<Observed, String> {
    // The engine's roots hold one CA. The rejected-certificate row has its
    // target present a leaf from a second one, which those roots do not know.
    let trusted = Ca::new().map_err(|err| err.to_string())?;
    let untrusted = match session.untrusted {
        true => Some(Ca::new().map_err(|err| err.to_string())?),
        false => None,
    };
    let issuer = untrusted.as_ref().unwrap_or(&trusted);
    let target = if session.refuse_dial {
        None
    } else {
        Some(Stub::start(&session.server_name, session.upstream.clone(), issuer).await?)
    };

    let mut roots = RootCertStore::empty();
    let engine_root: CertificateDer<'static> = trusted.root().clone();
    roots
        .add(engine_root.clone())
        .map_err(|err| err.to_string())?;
    let recorder = Arc::new(Recorder {
        events: Mutex::new(Vec::new()),
        deny_request: session
            .hooks
            .as_ref()
            .map(|s| s.deny_request)
            .unwrap_or(false),
        deny_response: session
            .hooks
            .as_ref()
            .map(|s| s.deny_response)
            .unwrap_or(false),
    });
    let hooks: Option<Arc<dyn Hooks>> = session
        .hooks
        .as_ref()
        .map(|_| Arc::clone(&recorder) as Arc<dyn Hooks>);
    let dialer: Arc<dyn UpstreamDialer> = Arc::new(ProbeDialer {
        address: target.as_ref().map(|stub| stub.address),
        refuse: session.refuse_dial,
    });
    let engine = Arc::new(
        Engine::new(&session.policy, hooks, dialer, Arc::new(roots))
            .map_err(|err| err.to_string())?,
    );

    let (client_io, engine_io) = tokio::io::duplex(PIPE);
    let serving = {
        let engine = Arc::clone(&engine);
        let name = session.server_name.clone();
        let dial = format!("{}:443", session.server_name);
        tokio::spawn(async move { engine.serve(engine_io, &name, &dial).await })
    };

    // The client trusts the engine's temporary root and offers what the row
    // says it offered, which is also what the handshake then negotiates.
    let mut client_roots = RootCertStore::empty();
    client_roots
        .add(engine.root())
        .map_err(|err| err.to_string())?;
    let mut config =
        ClientConfig::builder_with_provider(Arc::new(rustls::crypto::ring::default_provider()))
            .with_safe_default_protocol_versions()
            .map_err(|err| err.to_string())?
            .with_root_certificates(client_roots)
            .with_no_client_auth();
    if !session.negotiated.is_empty() {
        config.alpn_protocols = vec![session.negotiated.as_bytes().to_vec()];
    }
    let name = ServerName::try_from(session.server_name.clone()).map_err(|err| err.to_string())?;
    let stream = TlsConnector::from(Arc::new(config))
        .connect(name, client_io)
        .await
        .map_err(|err| format!("client handshake: {err}"))?;
    let negotiated = stream
        .get_ref()
        .1
        .alpn_protocol()
        .map(|protocol| String::from_utf8_lossy(protocol).into_owned())
        .unwrap_or_default();
    if negotiated != session.negotiated {
        return Err(format!(
            "the client offered {:?} and the engine negotiated {negotiated:?}",
            session.negotiated
        ));
    }

    let answers = tokio::time::timeout(
        SESSION_TIMEOUT,
        exchange(stream, &session.negotiated, &session.probes),
    )
    .await
    .map_err(|_| "the exchange did not finish".to_owned())??;
    let trace = recorder.snapshot();
    let seen = match &target {
        Some(stub) => stub.snapshot(),
        None => Vec::new(),
    };
    match tokio::time::timeout(SESSION_TIMEOUT, serving).await {
        Ok(Ok(Ok(()))) => {}
        Ok(Ok(Err(err))) => return Err(format!("the engine refused to serve: {err}")),
        Ok(Err(err)) => return Err(format!("the engine's task failed: {err}")),
        Err(_) => return Err("the engine did not return after the client closed".to_owned()),
    }
    Ok(Observed {
        trace,
        client: answers,
        seen,
    })
}

/// Send the row's requests over the negotiated protocol and collect the answers.
async fn exchange(
    stream: TlsStream<tokio::io::DuplexStream>,
    negotiated: &str,
    probes: &[Probe],
) -> Result<Vec<Answer>, String> {
    if negotiated == "h2" {
        return over_h2(stream, probes).await;
    }
    let mut wire = Raw {
        io: stream,
        buffer: Vec::new(),
    };
    let mut answers = Vec::new();
    for probe in probes {
        answers.push(wire.exchange(probe).await?);
    }
    // A client closes its side; the rows that follow a tunnel expect the engine
    // to come back rather than wait for a peer that never says goodbye.
    let _ = wire.io.shutdown().await;
    Ok(answers)
}

// ------------------------------------------------------------- the h1 client

/// A client that writes the request as bytes and reads the answer as bytes.
///
/// A library would answer "status, headers, body" and hide the question the
/// rows ask: how the answer was delimited. So the head is parsed here and the
/// framing is named from what arrived.
struct Raw {
    io: TlsStream<tokio::io::DuplexStream>,
    buffer: Vec<u8>,
}

impl Raw {
    async fn exchange(&mut self, probe: &Probe) -> Result<Answer, String> {
        let authority = if probe.authority.is_empty() {
            DEFAULT_SERVER_NAME
        } else {
            &probe.authority
        };
        let mut wire = Vec::new();
        wire.extend_from_slice(
            format!(
                "{} {} HTTP/1.1\r\nHost: {}\r\n",
                probe.method, probe.target, authority
            )
            .as_bytes(),
        );
        for (name, value) in &probe.headers {
            wire.extend_from_slice(format!("{name}: {value}\r\n").as_bytes());
        }
        if probe.pad_header > 0 {
            wire.extend_from_slice(b"x-pad: ");
            wire.extend(std::iter::repeat_n(b'x', probe.pad_header));
            wire.extend_from_slice(b"\r\n");
        }
        let body = probe.body.clone().unwrap_or_default();
        match probe.framing.as_str() {
            "length" => {
                wire.extend_from_slice(format!("Content-Length: {}\r\n", body.len()).as_bytes())
            }
            "chunked" => wire.extend_from_slice(b"Transfer-Encoding: chunked\r\n"),
            _ => {}
        }
        wire.extend_from_slice(b"\r\n");
        match probe.framing.as_str() {
            "length" => wire.extend_from_slice(&body),
            "chunked" => {
                wire.extend_from_slice(format!("{:x}\r\n", body.len()).as_bytes());
                wire.extend_from_slice(&body);
                wire.extend_from_slice(b"\r\n0\r\n\r\n");
            }
            _ => {}
        }
        // The oversized-header row is answered and closed while the client is
        // still writing, so a failed write is not a failed row: what the row
        // records is what came back.
        let _ = self.io.write_all(&wire).await;
        self.answer().await
    }

    async fn answer(&mut self) -> Result<Answer, String> {
        let status = self.status().await?;
        let headers = self.head().await?;
        let framing = framing_of(&headers);
        // 431 is answered by the stack, not by the engine: there is no framing
        // to compare and the body is that stack's business.
        if status == StatusCode::REQUEST_HEADER_FIELDS_TOO_LARGE.as_u16() {
            return Ok(Answer {
                status,
                headers: BTreeMap::new(),
                body: None,
                framing: None,
            });
        }
        let body = self.body(&framing, &headers).await?;
        Ok(Answer {
            status,
            headers,
            body: Some(body),
            framing: Some(framing),
        })
    }

    async fn status(&mut self) -> Result<u16, String> {
        let line = self.line().await?;
        let text = String::from_utf8_lossy(&line).into_owned();
        let mut parts = text.split(' ');
        let version = parts.next().unwrap_or_default();
        if !version.starts_with("HTTP/1.") {
            return Err(format!("the status line is {text:?}"));
        }
        parts
            .next()
            .and_then(|code| code.parse().ok())
            .ok_or_else(|| format!("the status line is {text:?}"))
    }

    async fn head(&mut self) -> Result<BTreeMap<String, Vec<String>>, String> {
        let mut heads: BTreeMap<String, Vec<String>> = BTreeMap::new();
        loop {
            let line = self.line().await?;
            if line.is_empty() {
                return Ok(heads);
            }
            let text = String::from_utf8_lossy(&line).into_owned();
            let (name, value) = text
                .split_once(':')
                .ok_or_else(|| format!("the header line is {text:?}"))?;
            let name = name.trim().to_ascii_lowercase();
            if MASKED.contains(&name.as_str()) {
                continue;
            }
            heads.entry(name).or_default().push(value.trim().to_owned());
        }
    }

    async fn body(
        &mut self,
        framing: &str,
        headers: &BTreeMap<String, Vec<String>>,
    ) -> Result<Vec<u8>, String> {
        if framing == "chunked" {
            let mut body = Vec::new();
            loop {
                let line = self.line().await?;
                let text = String::from_utf8_lossy(&line).into_owned();
                let size = text
                    .split(';')
                    .next()
                    .and_then(|size| usize::from_str_radix(size.trim(), 16).ok())
                    .ok_or_else(|| format!("the chunk size is {text:?}"))?;
                if size == 0 {
                    // Trailers, then the blank line that ends them.
                    while !self.line().await?.is_empty() {}
                    return Ok(body);
                }
                body.extend_from_slice(&self.exactly(size).await?);
                if !self.line().await?.is_empty() {
                    return Err("a chunk did not end with a blank line".to_owned());
                }
            }
        }
        if framing == "length" {
            let len = headers
                .get("content-length")
                .and_then(|values| values.first())
                .and_then(|value| value.parse::<usize>().ok())
                .unwrap_or(0);
            return self.exactly(len).await;
        }
        Ok(Vec::new())
    }

    async fn line(&mut self) -> Result<Vec<u8>, String> {
        loop {
            if let Some(end) = self.buffer.windows(2).position(|pair| pair == b"\r\n") {
                let line = self.buffer[..end].to_vec();
                self.buffer.drain(..end + 2);
                return Ok(line);
            }
            self.fill().await?;
        }
    }

    async fn exactly(&mut self, len: usize) -> Result<Vec<u8>, String> {
        while self.buffer.len() < len {
            self.fill().await?;
        }
        Ok(self.buffer.drain(..len).collect())
    }

    async fn fill(&mut self) -> Result<(), String> {
        let mut chunk = [0u8; 8 << 10];
        let read = self
            .io
            .read(&mut chunk)
            .await
            .map_err(|err| format!("read: {err}"))?;
        if read == 0 {
            return Err("the tunnel closed mid-answer".to_owned());
        }
        self.buffer.extend_from_slice(&chunk[..read]);
        Ok(())
    }
}

/// How a head delimited its body, named the way the dump names it: a length
/// only when it is a length a reader has to honour.
fn framing_of(headers: &BTreeMap<String, Vec<String>>) -> String {
    if headers.contains_key("transfer-encoding") {
        return "chunked".to_owned();
    }
    match headers
        .get("content-length")
        .and_then(|values| values.first())
        .and_then(|value| value.parse::<u64>().ok())
    {
        Some(len) if len > 0 => "length".to_owned(),
        _ => "none".to_owned(),
    }
}

// ------------------------------------------------------------- the h2 client

/// The same script over h2, which has no framing to choose: a row that names
/// chunked framing is not expressible here, and none does.
async fn over_h2(
    stream: TlsStream<tokio::io::DuplexStream>,
    probes: &[Probe],
) -> Result<Vec<Answer>, String> {
    let (mut sender, connection) =
        hyper::client::conn::http2::handshake(TokioExecutor::new(), TokioIo::new(stream))
            .await
            .map_err(|err| format!("h2 handshake: {err}"))?;
    let driving = tokio::spawn(async move {
        let _ = connection.await;
    });
    let mut answers = Vec::new();
    for probe in probes {
        let authority = if probe.authority.is_empty() {
            DEFAULT_SERVER_NAME
        } else {
            &probe.authority
        };
        let mut request = Request::builder()
            .method(Method::from_bytes(probe.method.as_bytes()).map_err(|err| err.to_string())?)
            .uri(format!("https://{authority}{}", probe.target));
        let head = request
            .headers_mut()
            .ok_or_else(|| "the client request builder refused its own headers".to_owned())?;
        for (name, value) in &probe.headers {
            head.append(
                header::HeaderName::from_bytes(name.as_bytes()).map_err(|err| err.to_string())?,
                HeaderValue::from_str(value).map_err(|err| err.to_string())?,
            );
        }
        let body = match probe.framing.as_str() {
            "length" => Bytes::from(probe.body.clone().unwrap_or_default()),
            _ => Bytes::new(),
        };
        let request = request
            .body(Full::new(body))
            .map_err(|err| err.to_string())?;
        let response = sender
            .send_request(request)
            .await
            .map_err(|err| format!("h2 round trip: {err}"))?;
        answers.push(from_response(response).await?);
    }
    drop(sender);
    let _ = tokio::time::timeout(SESSION_TIMEOUT, driving).await;
    Ok(answers)
}

async fn from_response(response: Response<Incoming>) -> Result<Answer, String> {
    let status = response.status().as_u16();
    let headers = heads(response.headers());
    let body = response
        .into_body()
        .collect()
        .await
        .map_err(|err| format!("h2 body: {err}"))?
        .to_bytes()
        .to_vec();
    Ok(Answer {
        status,
        headers: headers.clone(),
        body: Some(body),
        framing: Some(framing_of(&headers)),
    })
}

// ------------------------------------------------------------------ the hooks

/// The row's hooks: it writes down every call and decides by the script.
struct Recorder {
    events: Mutex<Vec<Event>>,
    deny_request: bool,
    deny_response: bool,
}

impl Recorder {
    fn snapshot(&self) -> Vec<Event> {
        self.events
            .lock()
            .map(|events| events.iter().map(clone_event).collect())
            .unwrap_or_default()
    }
}

impl Hooks for Recorder {
    fn on_request(&self, info: &RequestInfo) -> Result<(), Error> {
        if let Ok(mut events) = self.events.lock() {
            events.push(Event::Request {
                method: info.method.clone(),
                scheme: info.scheme.clone(),
                host: info.host.clone(),
                path: info.path.clone(),
                headers: heads(&info.header),
                body: info.body.as_ref().map(|body| body.to_vec()),
            });
        }
        if self.deny_request {
            return Err(Error::new(ErrorKind::PolicyDenied, DENIED));
        }
        Ok(())
    }

    fn on_response(&self, info: &ResponseInfo) -> Result<(), Error> {
        if let Ok(mut events) = self.events.lock() {
            events.push(Event::Response {
                status: info.status,
                headers: heads(&info.header),
                body: info.body.as_ref().map(|body| body.to_vec()),
            });
        }
        if self.deny_response {
            return Err(Error::new(ErrorKind::PolicyDenied, DENIED));
        }
        Ok(())
    }
}

/// What a refusing hook says. Its sentence never reaches the wire: the engine
/// answers with its own.
const DENIED: &str = "the harness denies this traffic";

fn clone_event(event: &Event) -> Event {
    match event {
        Event::Request {
            method,
            scheme,
            host,
            path,
            headers,
            body,
        } => Event::Request {
            method: method.clone(),
            scheme: scheme.clone(),
            host: host.clone(),
            path: path.clone(),
            headers: headers.clone(),
            body: body.clone(),
        },
        Event::Response {
            status,
            headers,
            body,
        } => Event::Response {
            status: *status,
            headers: headers.clone(),
            body: body.clone(),
        },
    }
}

// --------------------------------------------------------------- the dialer

/// The dialer the row needs: the target's address, or a refusal. The hostport
/// the engine hands over is ignored, as the dump's dialer ignores it.
struct ProbeDialer {
    address: Option<std::net::SocketAddr>,
    refuse: bool,
}

impl UpstreamDialer for ProbeDialer {
    fn dial<'a>(&'a self, hostport: &'a str) -> BoxFuture<'a, Result<Box<dyn Stream>, Error>> {
        Box::pin(async move {
            if self.refuse {
                return Err(Error::new(
                    ErrorKind::NotAvailable,
                    "the harness refuses the dial",
                ));
            }
            let address = self
                .address
                .ok_or_else(|| Error::new(ErrorKind::Internal, "the harness started no target"))?;
            let stream = TcpStream::connect(address)
                .await
                .map_err(|err| Error::new(ErrorKind::NotAvailable, format!("{hostport}: {err}")))?;
            Ok(Box::new(stream) as Box<dyn Stream>)
        })
    }
}

// --------------------------------------------------------------- the target

/// The target: a TLS server holding a certificate from the harness's own CA,
/// answering every request with the row's script and writing down what it saw.
struct Stub {
    address: std::net::SocketAddr,
    seen: Arc<Mutex<Vec<Seen>>>,
    serving: tokio::task::JoinHandle<()>,
}

impl Stub {
    async fn start(server_name: &str, script: Answer, issuer: &Ca) -> Result<Stub, String> {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .map_err(|err| format!("bind: {err}"))?;
        let address = listener.local_addr().map_err(|err| err.to_string())?;
        let config = issuer
            .server_config(server_name, &[])
            .map_err(|err| err.to_string())?;
        let acceptor = tokio_rustls::TlsAcceptor::from(config);
        let seen = Arc::new(Mutex::new(Vec::new()));
        let script = Arc::new(script);
        let serving = {
            let seen = Arc::clone(&seen);
            tokio::spawn(async move {
                loop {
                    let Ok((stream, _)) = listener.accept().await else {
                        return;
                    };
                    let acceptor = acceptor.clone();
                    let seen = Arc::clone(&seen);
                    let script = Arc::clone(&script);
                    tokio::spawn(async move {
                        let Ok(tls) = acceptor.accept(stream).await else {
                            return;
                        };
                        let sni = tls.get_ref().1.server_name().map(str::to_owned);
                        let service = service_fn(move |request: Request<Incoming>| {
                            let seen = Arc::clone(&seen);
                            let script = Arc::clone(&script);
                            let sni = sni.clone();
                            async move {
                                Ok::<_, std::convert::Infallible>(
                                    answer_target(request, &script, &seen, sni).await,
                                )
                            }
                        });
                        let _ = hyper::server::conn::http1::Builder::new()
                            .serve_connection(TokioIo::new(tls), service)
                            .await;
                    });
                }
            })
        };
        Ok(Stub {
            address,
            seen,
            serving,
        })
    }

    fn snapshot(&self) -> Vec<Seen> {
        self.seen
            .lock()
            .map(|seen| {
                seen.iter()
                    .map(|row| Seen {
                        method: row.method.clone(),
                        target: row.target.clone(),
                        authority: row.authority.clone(),
                        sni: row.sni.clone(),
                        proto: row.proto.clone(),
                        headers: row.headers.clone(),
                        body: row.body.clone(),
                        framing: row.framing.clone(),
                    })
                    .collect()
            })
            .unwrap_or_default()
    }
}

impl Drop for Stub {
    fn drop(&mut self) {
        self.serving.abort();
    }
}

/// Record one request and answer it with the script.
async fn answer_target(
    request: Request<Incoming>,
    script: &Answer,
    seen: &Arc<Mutex<Vec<Seen>>>,
    sni: Option<String>,
) -> Response<Scripted> {
    let (parts, incoming) = request.into_parts();
    // The framing is read off the whole head -- `transfer-encoding` is what
    // names it -- and then dropped from the record, because core's server moves
    // it out of the map it shows a reader.
    let mut head = heads(&parts.headers);
    let framing = framing_of(&head);
    head.remove("host");
    head.remove("transfer-encoding");
    let authority = parts
        .uri
        .authority()
        .map(|authority| authority.to_string())
        .or_else(|| {
            parts
                .headers
                .get(header::HOST)
                .and_then(|value| value.to_str().ok())
                .map(str::to_owned)
        })
        .unwrap_or_default();
    let target = parts
        .uri
        .path_and_query()
        .map(|target| target.as_str().to_owned())
        .unwrap_or_else(|| "/".to_owned());
    let body = read_all(incoming, BODY_CAP).await;
    if let Ok(mut rows) = seen.lock() {
        rows.push(Seen {
            method: parts.method.as_str().to_owned(),
            target,
            authority,
            sni,
            proto: format!("{:?}", parts.version),
            headers: head,
            body: Some(body.clone()),
            framing,
        });
    }
    // `status: 0` in the script is "no answer was scripted"; the dump writes a
    // `204` for such a row.
    let status = if script.status == 0 {
        StatusCode::NO_CONTENT
    } else {
        StatusCode::from_u16(script.status).unwrap_or(StatusCode::OK)
    };
    let mut response = Response::builder().status(status);
    if let Some(headers) = response.headers_mut() {
        for (name, values) in &script.headers {
            if let Ok(name) = header::HeaderName::from_bytes(name.as_bytes()) {
                for value in values {
                    if let Ok(value) = HeaderValue::from_str(value) {
                        headers.append(name.clone(), value);
                    }
                }
            }
        }
    }
    let unknown = script.framing.as_deref() == Some("chunked");
    let body = script.body.clone().unwrap_or_default();
    response
        .body(Scripted::new(body, unknown))
        .unwrap_or_else(|_| Response::new(Scripted::new(Vec::new(), false)))
}

/// A body with a length the writer may or may not know: unknown is what makes
/// hyper delimit it with chunks, and the row that scripts chunked framing needs
/// exactly that.
struct Scripted {
    body: Option<Bytes>,
    unknown: bool,
}

impl Scripted {
    fn new(body: Vec<u8>, unknown: bool) -> Scripted {
        Scripted {
            body: if body.is_empty() && !unknown {
                None
            } else {
                Some(Bytes::from(body))
            },
            unknown,
        }
    }
}

impl HttpBody for Scripted {
    type Data = Bytes;
    type Error = std::convert::Infallible;

    fn poll_frame(
        self: std::pin::Pin<&mut Self>,
        _cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Result<Frame<Bytes>, Self::Error>>> {
        let this = self.get_mut();
        match this.body.take() {
            Some(data) => std::task::Poll::Ready(Some(Ok(Frame::data(data)))),
            None => std::task::Poll::Ready(None),
        }
    }

    fn size_hint(&self) -> SizeHint {
        if self.unknown {
            SizeHint::default()
        } else {
            SizeHint::with_exact(
                self.body
                    .as_ref()
                    .map(|body| body.len() as u64)
                    .unwrap_or(0),
            )
        }
    }
}

// ------------------------------------------------------------------- the shapes

fn contract() -> Value {
    match serde_json::from_str(CONTRACT) {
        Ok(document) => document,
        Err(err) => panic!("the committed mitm contract does not parse: {err}"),
    }
}

fn rows<'a>(document: &'a Value, table: &str) -> &'a [Value] {
    match document.get(table).and_then(Value::as_array) {
        Some(rows) => rows,
        None => panic!("the document has no {table} table"),
    }
}

fn string(row: &Value, field: &str) -> String {
    row.get(field)
        .and_then(Value::as_str)
        .unwrap_or_else(|| panic!("a row has no {field}: {row}"))
        .to_owned()
}

fn probes(row: &Value) -> Vec<Probe> {
    row.get("requests")
        .and_then(Value::as_array)
        .map(|requests| {
            requests
                .iter()
                .map(|request| Probe {
                    method: string(request, "method"),
                    target: string(request, "target"),
                    authority: request
                        .get("authority")
                        .and_then(Value::as_str)
                        .unwrap_or_default()
                        .to_owned(),
                    headers: request.get("headers").map(pairs).unwrap_or_default(),
                    body: body(request),
                    framing: string(request, "framing"),
                    pad_header: request
                        .get("pad_header")
                        .and_then(Value::as_u64)
                        .unwrap_or(0) as usize,
                })
                .collect()
        })
        .unwrap_or_default()
}

/// A scripted header map, in the order the dump sorted it: by name, values in
/// the order they were added.
fn pairs(value: &Value) -> Vec<(String, String)> {
    let mut pairs = Vec::new();
    if let Some(map) = value.as_object() {
        let mut names: Vec<&String> = map.keys().collect();
        names.sort();
        for name in names {
            if MASKED.contains(&name.as_str()) {
                continue;
            }
            for value in map
                .get(name)
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                pairs.push((name.clone(), value.as_str().unwrap_or_default().to_owned()));
            }
        }
    }
    pairs
}

fn answer(value: &Value) -> Answer {
    Answer {
        status: value.get("status").and_then(Value::as_u64).unwrap_or(0) as u16,
        headers: value.get("headers").map(map).unwrap_or_default(),
        body: body(value),
        framing: value
            .get("framing")
            .and_then(Value::as_str)
            .map(str::to_owned),
    }
}

fn seen_row(value: &Value) -> Seen {
    Seen {
        method: string(value, "method"),
        target: string(value, "target"),
        authority: string(value, "authority"),
        sni: value.get("sni").and_then(Value::as_str).map(str::to_owned),
        proto: string(value, "proto"),
        headers: value.get("headers").map(map).unwrap_or_default(),
        body: body(value),
        framing: string(value, "framing"),
    }
}

fn event(value: &Value) -> Event {
    let headers = value.get("headers").map(map).unwrap_or_default();
    let body = body(value);
    if value.get("event").and_then(Value::as_str) == Some("response") {
        return Event::Response {
            status: value.get("status").and_then(Value::as_u64).unwrap_or(0) as u16,
            headers,
            body,
        };
    }
    Event::Request {
        method: string(value, "method"),
        scheme: string(value, "scheme"),
        host: string(value, "host"),
        path: string(value, "path"),
        headers,
        body,
    }
}

fn map(value: &Value) -> BTreeMap<String, Vec<String>> {
    let mut out = BTreeMap::new();
    if let Some(heads) = value.as_object() {
        for (name, values) in heads {
            if MASKED.contains(&name.as_str()) {
                continue;
            }
            let values: Vec<String> = values
                .as_array()
                .map(|values| {
                    values
                        .iter()
                        .filter_map(Value::as_str)
                        .map(str::to_owned)
                        .collect()
                })
                .unwrap_or_default();
            out.insert(name.clone(), values);
        }
    }
    out
}

/// A body as the dump spells it: `null` is "the hook was given no body", `""` is
/// a body that was given and was empty, and anything else is hex.
fn body(value: &Value) -> Option<Vec<u8>> {
    match value.get("body") {
        Some(value) if !value.is_null() => Some(hex(value)),
        _ => None,
    }
}

/// A body's hex digits.
fn hex(value: &Value) -> Vec<u8> {
    let text = value.as_str().unwrap_or_default();
    let digits = text.as_bytes();
    if !digits.len().is_multiple_of(2) {
        panic!("a body is not an even number of hex digits: {text:?}");
    }
    let mut bytes = Vec::with_capacity(digits.len() / 2);
    for pair in digits.chunks(2) {
        let pair = std::str::from_utf8(pair).unwrap_or_default();
        match u8::from_str_radix(pair, 16) {
            Ok(byte) => bytes.push(byte),
            Err(err) => panic!("a body is not hex: {text:?} ({err})"),
        }
    }
    bytes
}

/// The headers a comparison sees: the stack's map minus the authority and the
/// framing header, lowercased and sorted, with the masked names gone.
fn heads(headers: &HeaderMap) -> BTreeMap<String, Vec<String>> {
    let mut out: BTreeMap<String, Vec<String>> = BTreeMap::new();
    for name in headers.keys() {
        let name = name.as_str().to_ascii_lowercase();
        if MASKED.contains(&name.as_str()) {
            continue;
        }
        let values: Vec<String> = headers
            .get_all(name.as_str())
            .iter()
            .map(|value| String::from_utf8_lossy(value.as_bytes()).into_owned())
            .collect();
        out.entry(name).or_default().extend(values);
    }
    out
}

async fn read_all<B>(body: B, cap: u64) -> Vec<u8>
where
    B: HttpBody<Data = Bytes> + Unpin,
{
    let mut collected = Vec::new();
    let mut body = body;
    while let Some(frame) = body.frame().await {
        let Ok(frame) = frame else { break };
        if let Ok(data) = frame.into_data() {
            collected.extend_from_slice(&data);
            if collected.len() as u64 > cap {
                break;
            }
        }
    }
    collected
}

fn report(problems: Vec<String>, table: &str) {
    if problems.is_empty() {
        return;
    }
    panic!(
        "{} of the committed {table} rows do not replay:\n{}",
        problems.len(),
        problems.join("\n")
    );
}

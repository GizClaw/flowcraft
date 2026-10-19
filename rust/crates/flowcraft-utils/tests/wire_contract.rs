//! Replays the committed wire contract against the port.
//!
//! `rust/conformance/wire-contract.json` is dumped from core by
//! `tools/conformance-oracle -wire-contract` and checked in; the gate fails if
//! regenerating it produces anything else. This test is the other half: every
//! scripted SOCKS5 session is run, every `CONNECT` is answered, every upstream
//! is dialed, and the bytes and audit records are compared with core's.
//!
//! The rows exist because each of these is a place a port looks right on the
//! wire and is wrong:
//!
//!   - which bytes answer a greeting, a refusal, a bad command, and a request
//!     the server deliberately answers with silence;
//!   - which decisions are audited, and which are audited *before* they are
//!     judged (core's SOCKS5 path records the deny it could not make);
//!   - what an upstream sees: the `Host` line repeated, the credentials line
//!     present exactly when the URL had userinfo, and the SOCKS5 handshake;
//!   - what a target that speaks first puts on the tunnel, which is the bytes
//!     an upstream sent after its `200`.

use std::net::SocketAddr;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use flowcraft_utils::net::BoxFuture;
use flowcraft_utils::net::dial::{Dialer, Upstream};
use flowcraft_utils::net::policy::{MitmPolicy, NetAction, NetMode, NetRule};
use flowcraft_utils::net::proxy::{Audit, Proxy, ProxyConfig};
use flowcraft_utils::net::serve::{self, Hooks, HttpHandler, Outcome, Server};
use flowcraft_utils::net::socks5::{self, Connector, Stream, Target};
use serde_json::Value;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

const CONTRACT: &str = include_str!("../../../conformance/wire-contract.json");

/// How long a session may say nothing before its answer is called complete.
/// Every row is loopback, so the window only waits for a session that has
/// nothing more to say.
const QUIET: Duration = Duration::from_millis(400);

#[tokio::test]
async fn the_wire_contract_replays() {
    let document = contract();
    let mut failures = Vec::new();

    for row in rows(&document, "socks5") {
        replay_socks_row(row, &mut failures).await;
    }
    for row in rows(&document, "http") {
        replay_http_row(row, &mut failures).await;
    }
    for row in rows(&document, "upstream") {
        replay_upstream_row(row, &mut failures).await;
    }

    if !failures.is_empty() {
        panic!(
            "{} of the committed wire rows do not replay:\n{}",
            failures.len(),
            failures.join("\n")
        );
    }
}

/// The contract is a set of tables, and an empty one would replay perfectly.
#[test]
fn the_contract_still_covers_every_table() {
    let document = contract();
    for (table, least) in [("socks5", 16), ("http", 6), ("upstream", 5)] {
        let count = rows(&document, table).len();
        assert!(
            count >= least,
            "{table} has {count} rows, expected at least {least}"
        );
    }
}

// ------------------------------------------------------------------ the rows

async fn replay_socks_row(row: &Value, failures: &mut Vec<String>) {
    let name = text(row, "name");
    let client = bytes(row, "client");
    let expected = bytes(row, "reply");

    let stub = match upstream_spec(row) {
        Some(spec) => match Stub::start(spec).await {
            Ok(stub) => Some(stub),
            Err(err) => {
                failures.push(format!("{name}: start the upstream: {err}"));
                return;
            }
        },
        None => None,
    };
    let mut config = config_of(row);
    if let Some(stub) = &stub {
        config.upstream = stub.url();
    }
    let (server, seen) = server_of(row, config, stub.is_some());

    let bound = match serve::bind_tcp(server).await {
        Ok(bound) => Arc::new(bound),
        Err(err) => {
            failures.push(format!("{name}: bind: {err}"));
            return;
        }
    };
    let address = match bound.addr() {
        Some(address) => address,
        None => {
            failures.push(format!("{name}: a tcp listener has an address"));
            return;
        }
    };
    let serving = {
        let bound = Arc::clone(&bound);
        tokio::spawn(async move { bound.serve().await })
    };

    match session(address, &client).await {
        Ok(reply) => {
            let actual = if flag(row, "mask_bind") {
                mask_bind(reply)
            } else {
                reply
            };
            if actual != expected {
                failures.push(format!(
                    "{name}: reply is {} not {}",
                    to_hex(&actual),
                    to_hex(&expected)
                ));
            }
        }
        Err(err) => failures.push(format!("{name}: session: {err}")),
    }

    check_audit(name, row, &seen, failures);
    if let Some(stub) = stub {
        stub.check_request(name, &bytes(row, "upstream_request"), failures);
    }
    bound.shutdown();
    serving.abort();
}

async fn replay_http_row(row: &Value, failures: &mut Vec<String>) {
    let name = text(row, "name");
    let request = text(row, "request");
    let authority = match authority_of(request) {
        Some(authority) => authority,
        None => {
            failures.push(format!("{name}: the request is not a CONNECT"));
            return;
        }
    };
    let stub = match upstream_spec(row) {
        Some(spec) => match Stub::start(spec).await {
            Ok(stub) => Some(stub),
            Err(err) => {
                failures.push(format!("{name}: start the upstream: {err}"));
                return;
            }
        },
        None => None,
    };
    let mut config = config_of(row);
    if let Some(stub) = &stub {
        config.upstream = stub.url();
    }
    let (server, seen) = server_of(row, config, stub.is_some());

    let (service, mut client) = tokio::io::duplex(64 * 1024);
    let handler = tokio::spawn(async move {
        let mut service = service;
        server.handle_connect(&mut service, &authority).await
    });

    let status = number(row, "status");
    if status == 200 {
        let head = read_head(&mut client).await;
        let expected = bytes(row, "head");
        if head != expected {
            failures.push(format!(
                "{name}: head is {:?} not {:?}",
                String::from_utf8_lossy(&head),
                String::from_utf8_lossy(&expected)
            ));
        }
        let payload = bytes(row, "payload");
        if !payload.is_empty()
            && let Err(err) = client.write_all(&payload).await
        {
            failures.push(format!("{name}: write the payload: {err}"));
        }
        let tunnel = read_until_quiet(&mut client).await;
        drop(client);
        handler.abort();
        let expected = bytes(row, "body");
        if tunnel != expected {
            failures.push(format!(
                "{name}: tunnel is {} not {}",
                to_hex(&tunnel),
                to_hex(&expected)
            ));
        }
    } else {
        match handler.await {
            Ok(Outcome::Refused(refusal)) => {
                if i64::from(refusal.status) != status {
                    failures.push(format!(
                        "{name}: refused with {} not {status}",
                        refusal.status
                    ));
                }
                let body = bytes(row, "body");
                if refusal.body.as_bytes() != body.as_slice() {
                    failures.push(format!(
                        "{name}: body is {:?} not {:?}",
                        refusal.body,
                        String::from_utf8_lossy(&body)
                    ));
                }
                // A refusal is the caller's to write: nothing may reach the
                // client on this path.
                let written = read_until_quiet(&mut client).await;
                if !written.is_empty() {
                    failures.push(format!(
                        "{name}: the refusal wrote {} to the client",
                        to_hex(&written)
                    ));
                }
            }
            Ok(other) => failures.push(format!("{name}: expected a refusal, got {other:?}")),
            Err(err) => failures.push(format!("{name}: handler task: {err}")),
        }
    }

    check_audit(name, row, &seen, failures);
    if let Some(stub) = stub {
        stub.check_request(name, &bytes(row, "upstream_request"), failures);
    }
}

async fn replay_upstream_row(row: &Value, failures: &mut Vec<String>) {
    let name = text(row, "name");
    // This table describes the upstream in the row itself: there is no client
    // session in front of it, so there is no nested spec to read.
    let spec = stub_spec(row);
    let stub = match Stub::start(&spec).await {
        Ok(stub) => stub,
        Err(err) => {
            failures.push(format!("{name}: start the upstream: {err}"));
            return;
        }
    };
    let dialer = match Upstream::parse(&stub.url()) {
        Ok(upstream) => Dialer::new(upstream),
        Err(err) => {
            failures.push(format!("{name}: parse the upstream URL: {err}"));
            return;
        }
    };
    let (host, port) = split_target(text(row, "target"));
    let target = Target { host, port };
    match dialer.connect(&target).await {
        Ok(mut dialed) => {
            // A target that speaks first puts its greeting on the tunnel
            // before the dialer ever writes.
            let first = read_until_quiet(&mut dialed.stream).await;
            let expected = bytes(row, "tunnel");
            if first != expected {
                failures.push(format!(
                    "{name}: tunnel is {} not {}",
                    to_hex(&first),
                    to_hex(&expected)
                ));
            }
        }
        Err(err) => failures.push(format!("{name}: dial: {err}")),
    }
    stub.check_request(name, &bytes(row, "request"), failures);
}

// ------------------------------------------------------------------ the stubs

/// The far end a row dials through: it records what it receives, answers the
/// way the row asks, and relays whatever follows.
struct Stub {
    address: SocketAddr,
    spec: Value,
    seen: Arc<Mutex<Vec<u8>>>,
}

impl Stub {
    async fn start(spec: &Value) -> Result<Stub, String> {
        let listener = TcpListener::bind(("127.0.0.1", 0))
            .await
            .map_err(|err| err.to_string())?;
        let address = listener.local_addr().map_err(|err| err.to_string())?;
        let seen = Arc::new(Mutex::new(Vec::new()));
        let stub = Stub {
            address,
            spec: spec.clone(),
            seen: Arc::clone(&seen),
        };
        let script = bytes(spec, "script");
        let echo = flag(spec, "echo");
        let require_auth = flag(spec, "require_auth");
        let scheme = text(spec, "scheme").to_owned();
        tokio::spawn(async move {
            let accepted = listener.accept().await;
            let (mut stream, _) = match accepted {
                Ok(accepted) => accepted,
                Err(_) => return,
            };
            let mut recorder = Recorder {
                stream: &mut stream,
                seen: &seen,
            };
            match scheme.as_str() {
                "http" => recorder.serve_http(&script, echo).await,
                "socks5" => recorder.serve_socks5(require_auth, echo).await,
                _ => {}
            }
        });
        Ok(stub)
    }

    fn url(&self) -> String {
        let scheme = text(&self.spec, "scheme");
        let userinfo = match optional(&self.spec, "user") {
            Some(user) => {
                // The password rides in the same string, present or not: a URL
                // written "user:@host" and one written "user@host" are the same
                // upstream to core.
                format!("{user}:{}@", optional(&self.spec, "password").unwrap_or(""))
            }
            None => String::new(),
        };
        format!("{scheme}://{userinfo}{}", self.address)
    }

    fn check_request(&self, name: &str, expected: &[u8], failures: &mut Vec<String>) {
        let seen = match self.seen.lock() {
            Ok(seen) => seen.clone(),
            Err(_) => {
                failures.push(format!("{name}: the stub's record is poisoned"));
                return;
            }
        };
        if seen != expected {
            failures.push(format!(
                "{name}: the upstream saw {} not {}",
                to_hex(&seen),
                to_hex(expected)
            ));
        }
    }
}

/// A connection the stub reads from, with everything it reads recorded.
struct Recorder<'a> {
    stream: &'a mut TcpStream,
    seen: &'a Mutex<Vec<u8>>,
}

impl Recorder<'_> {
    fn note(&self, chunk: &[u8]) {
        if let Ok(mut seen) = self.seen.lock() {
            seen.extend_from_slice(chunk);
        }
    }

    async fn read_into(&mut self, buffer: &mut [u8]) -> Result<(), String> {
        self.stream
            .read_exact(buffer)
            .await
            .map_err(|err| err.to_string())?;
        self.note(buffer);
        Ok(())
    }

    async fn serve_http(&mut self, script: &[u8], echo: bool) {
        let mut head = Vec::new();
        let mut byte = [0u8; 1];
        loop {
            if self.stream.read(&mut byte).await.unwrap_or(0) == 0 {
                return;
            }
            head.push(byte[0]);
            if head.ends_with(b"\r\n\r\n") {
                break;
            }
        }
        self.note(&head);
        if self.stream.write_all(script).await.is_err() {
            return;
        }
        if echo {
            self.echo().await;
        }
    }

    async fn serve_socks5(&mut self, require_auth: bool, echo: bool) {
        let mut head = [0u8; 2];
        if self.read_into(&mut head).await.is_err() {
            return;
        }
        let mut methods = vec![0u8; usize::from(head[1])];
        if self.read_into(&mut methods).await.is_err() {
            return;
        }
        let offers_auth = methods.contains(&socks5::USER_PASS);
        let chosen = if require_auth && offers_auth {
            socks5::USER_PASS
        } else {
            socks5::NO_AUTH
        };
        if self
            .stream
            .write_all(&[socks5::VERSION, chosen])
            .await
            .is_err()
        {
            return;
        }
        if chosen == socks5::USER_PASS {
            // RFC 1929: VER, ULEN, UNAME, PLEN, PASSWD.
            let mut auth = [0u8; 2];
            if self.read_into(&mut auth).await.is_err() {
                return;
            }
            let mut username = vec![0u8; usize::from(auth[1])];
            if self.read_into(&mut username).await.is_err() {
                return;
            }
            let mut length = [0u8; 1];
            if self.read_into(&mut length).await.is_err() {
                return;
            }
            let mut password = vec![0u8; usize::from(length[0])];
            if self.read_into(&mut password).await.is_err() {
                return;
            }
            if self.stream.write_all(&[0x01, 0x00]).await.is_err() {
                return;
            }
        }
        let mut request = [0u8; 4];
        if self.read_into(&mut request).await.is_err() {
            return;
        }
        let mut address = match request[3] {
            socks5::ATYP_IPV4 => vec![0u8; 4],
            socks5::ATYP_IPV6 => vec![0u8; 16],
            _ => {
                let mut length = [0u8; 1];
                if self.read_into(&mut length).await.is_err() {
                    return;
                }
                vec![0u8; usize::from(length[0])]
            }
        };
        if self.read_into(&mut address).await.is_err() {
            return;
        }
        let mut port = [0u8; 2];
        if self.read_into(&mut port).await.is_err() {
            return;
        }
        let reply = [
            socks5::VERSION,
            socks5::SUCCESS,
            0x00,
            socks5::ATYP_IPV4,
            0,
            0,
            0,
            0,
            0,
            0,
        ];
        if self.stream.write_all(&reply).await.is_err() {
            return;
        }
        if echo {
            self.echo().await;
        }
    }

    /// Relay whatever arrives back to the sender. Nothing is recorded: the
    /// row's `request` is what the upstream received, not what it said back.
    async fn echo(&mut self) {
        let mut buffer = [0u8; 4096];
        loop {
            match self.stream.read(&mut buffer).await {
                Ok(0) | Err(_) => return,
                Ok(read) => {
                    let mut chunk = [0u8; 4096];
                    let length = read;
                    chunk[..length].copy_from_slice(&buffer[..length]);
                    if self.stream.write_all(&chunk[..length]).await.is_err() {
                        return;
                    }
                }
            }
        }
    }
}

// ------------------------------------------------------------------ plumbing

fn contract() -> Value {
    match serde_json::from_str(CONTRACT) {
        Ok(value) => value,
        Err(err) => panic!("the committed contract does not decode: {err}"),
    }
}

fn rows<'a>(document: &'a Value, table: &str) -> &'a Vec<Value> {
    match document[table].as_array() {
        Some(rows) => rows,
        None => panic!("{table} is not an array"),
    }
}

fn text<'a>(row: &'a Value, field: &str) -> &'a str {
    match row[field].as_str() {
        Some(value) => value,
        None => panic!("{field} is not a string in {row}"),
    }
}

fn optional<'a>(row: &'a Value, field: &str) -> Option<&'a str> {
    row[field].as_str()
}

fn flag(row: &Value, field: &str) -> bool {
    row[field].as_bool().unwrap_or_default()
}

fn number(row: &Value, field: &str) -> i64 {
    match row[field].as_i64() {
        Some(value) => value,
        None => panic!("{field} is not a number in {row}"),
    }
}

fn bytes(row: &Value, field: &str) -> Vec<u8> {
    match optional(row, field) {
        Some(encoded) => from_hex(encoded),
        None => Vec::new(),
    }
}

fn from_hex(encoded: &str) -> Vec<u8> {
    let digits = encoded.as_bytes();
    let mut out = Vec::with_capacity(digits.len() / 2);
    let mut index = 0;
    while index + 1 < digits.len() {
        out.push(hex_digit(digits[index]) << 4 | hex_digit(digits[index + 1]));
        index += 2;
    }
    out
}

fn hex_digit(byte: u8) -> u8 {
    match byte {
        b'0'..=b'9' => byte - b'0',
        b'a'..=b'f' => byte - b'a' + 10,
        other => panic!("not a hex digit: {:?}", other as char),
    }
}

fn to_hex(bytes: &[u8]) -> String {
    let mut out = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        out.push_str(&format!("{byte:02x}"));
    }
    out
}

/// The config a row describes: its mode, rules and allow-list.
fn config_of(row: &Value) -> ProxyConfig {
    ProxyConfig {
        mode: mode_of(row),
        allow_hosts: strings(row, "allow_hosts"),
        rules: rules(row),
        upstream: String::new(),
        mitm: None::<MitmPolicy>,
    }
}

fn mode_of(row: &Value) -> NetMode {
    match text(row, "mode") {
        "default" => NetMode::Default,
        "deny_all" => NetMode::DenyAll,
        "allow_list" => NetMode::AllowList,
        "proxy" => NetMode::Proxy,
        other => panic!("unknown mode {other:?} in {row}"),
    }
}

fn mode_name(mode: NetMode) -> &'static str {
    match mode {
        NetMode::Default => "default",
        NetMode::DenyAll => "deny_all",
        NetMode::AllowList => "allow_list",
        NetMode::Proxy => "proxy",
    }
}

fn strings(row: &Value, field: &str) -> Vec<String> {
    match row[field].as_array() {
        Some(values) => values
            .iter()
            .map(|value| match value.as_str() {
                Some(text) => text.to_owned(),
                None => panic!("{field} holds a non-string in {row}"),
            })
            .collect(),
        None => Vec::new(),
    }
}

fn rules(row: &Value) -> Vec<NetRule> {
    match row["rules"].as_array() {
        Some(values) => values
            .iter()
            .map(|rule| NetRule {
                action: match text(rule, "action") {
                    "allow" => NetAction::Allow,
                    "deny" => NetAction::Deny,
                    other => panic!("unknown action {other:?} in {rule}"),
                },
                host: text(rule, "host").to_owned(),
                port: i32::try_from(number(rule, "port")).unwrap_or_default(),
                reason: optional(rule, "reason").unwrap_or_default().to_owned(),
            })
            .collect(),
        None => Vec::new(),
    }
}

/// The stub spec for an upstream row: the same shape a nested `upstream`
/// object has, built from the row's own fields.
fn stub_spec(row: &Value) -> Value {
    let mut spec = serde_json::Map::new();
    spec.insert("scheme".to_owned(), row["scheme"].clone());
    for field in ["user", "password", "script"] {
        if let Some(value) = optional(row, field) {
            spec.insert(field.to_owned(), Value::String(value.to_owned()));
        }
    }
    if flag(row, "require_auth") {
        spec.insert("require_auth".to_owned(), Value::Bool(true));
    }
    Value::Object(spec)
}

fn upstream_spec(row: &Value) -> Option<&Value> {
    match row.get("upstream") {
        Some(spec) if !spec.is_null() => Some(spec),
        _ => None,
    }
}

/// A server over the row's policy, with its decisions recorded and the row's
/// hook when it has one.
fn server_of(
    row: &Value,
    config: ProxyConfig,
    has_upstream: bool,
) -> (Server, Arc<Mutex<Vec<Audit>>>) {
    let dialer = if has_upstream {
        // The upstream URL is set on the config, and the row's server dials
        // through it.
        match Upstream::parse(&config.upstream) {
            Ok(upstream) => Dialer::new(upstream),
            Err(err) => panic!("the row's upstream does not parse: {err}"),
        }
    } else {
        Dialer::direct()
    };
    let proxy = Proxy::new(&config);
    let seen = Arc::new(Mutex::new(Vec::new()));
    let recorder = Arc::clone(&seen);
    let sink: Arc<dyn serve::AuditSink> = Arc::new(move |audit: &Audit| {
        if let Ok(mut log) = recorder.lock() {
            log.push(audit.clone());
        }
    });
    let http: Arc<dyn HttpHandler> =
        Arc::new(|_connection: Box<dyn Stream>| -> BoxFuture<'static, ()> { Box::pin(async {}) });
    let mut server = Server::new(proxy, dialer, http).with_audit(sink);
    if flag(row, "hooks") {
        server = server.with_hooks(Arc::new(RefuseHook));
    }
    (server, seen)
}

/// Blocks every destination, which is what a hook is for.
struct RefuseHook;

impl Hooks for RefuseHook {
    fn on_connect(&self, _host: &str, _port: i32) -> Result<(), String> {
        Err("refused by the conformance hook".to_owned())
    }
}

fn check_audit(name: &str, row: &Value, seen: &Mutex<Vec<Audit>>, failures: &mut Vec<String>) {
    let expected = match row["audit"].as_array() {
        Some(rows) => rows.clone(),
        None => Vec::new(),
    };
    let actual = match seen.lock() {
        Ok(log) => log.clone(),
        Err(_) => {
            failures.push(format!("{name}: the audit log is poisoned"));
            return;
        }
    };
    if actual.len() != expected.len() {
        failures.push(format!(
            "{name}: {} audit records, expected {}: {:?}",
            actual.len(),
            expected.len(),
            actual
        ));
        return;
    }
    for (index, (record, want)) in actual.iter().zip(expected.iter()).enumerate() {
        let wanted = Audit {
            host: text(want, "host").to_owned(),
            port: i32::try_from(number(want, "port")).unwrap_or_default(),
            action: match text(want, "action") {
                "allow" => NetAction::Allow,
                "deny" => NetAction::Deny,
                other => panic!("unknown action {other:?} in {want}"),
            },
            mode: match text(want, "mode") {
                "default" => NetMode::Default,
                "deny_all" => NetMode::DenyAll,
                "allow_list" => NetMode::AllowList,
                "proxy" => NetMode::Proxy,
                other => panic!("unknown mode {other:?} in {want}"),
            },
            rule: text(want, "rule").to_owned(),
            reason: optional(want, "reason").unwrap_or_default().to_owned(),
            dial_host: text(want, "dial_host").to_owned(),
        };
        if *record != wanted {
            failures.push(format!(
                "{name}: audit record {index} is {record:?}, expected {wanted:?} ({} mode)",
                mode_name(record.mode)
            ));
        }
    }
}

/// Zero BND.ADDR and BND.PORT in a success reply and keep ATYP: the address is
/// the outbound socket's local one, i.e. an ephemeral port, while ATYP is core
/// choosing IPv4 or IPv6 for it.
fn mask_bind(reply: Vec<u8>) -> Vec<u8> {
    const METHOD_REPLY: usize = 2;
    if reply.len() < METHOD_REPLY + 4 || reply[0] != socks5::VERSION {
        return reply;
    }
    let request = &reply[METHOD_REPLY..];
    if request[0] != socks5::VERSION || request[1] != socks5::SUCCESS {
        return reply;
    }
    let address = match request[3] {
        socks5::ATYP_IPV4 => 4,
        socks5::ATYP_IPV6 => 16,
        _ => return reply,
    };
    let start = METHOD_REPLY + 4;
    let end = start + address + 2;
    if reply.len() < end {
        return reply;
    }
    let mut masked = reply;
    for byte in &mut masked[start..end] {
        *byte = 0;
    }
    masked
}

/// Write one client script, then read until the session closes or goes quiet.
async fn session(address: SocketAddr, client: &[u8]) -> Result<Vec<u8>, String> {
    let mut stream = TcpStream::connect(address)
        .await
        .map_err(|err| err.to_string())?;
    stream
        .write_all(client)
        .await
        .map_err(|err| err.to_string())?;
    let reply = read_until_quiet(&mut stream).await;
    let _ = stream.shutdown().await;
    Ok(reply)
}

async fn read_until_quiet<S>(stream: &mut S) -> Vec<u8>
where
    S: AsyncRead + Unpin,
{
    let mut out = Vec::new();
    let mut buffer = [0u8; 4096];
    loop {
        match tokio::time::timeout(QUIET, stream.read(&mut buffer)).await {
            Err(_) | Ok(Ok(0)) | Ok(Err(_)) => break,
            Ok(Ok(read)) => out.extend_from_slice(&buffer[..read]),
        }
    }
    out
}

/// Read a response head, terminator included, or nothing if none arrives.
async fn read_head<S>(stream: &mut S) -> Vec<u8>
where
    S: AsyncRead + Unpin,
{
    let mut out = Vec::new();
    let mut byte = [0u8; 1];
    loop {
        match tokio::time::timeout(QUIET, stream.read(&mut byte)).await {
            Ok(Ok(1)) => {
                out.push(byte[0]);
                if out.ends_with(b"\r\n\r\n") {
                    return out;
                }
            }
            _ => return out,
        }
    }
}

/// The authority a `CONNECT` request line names.
fn authority_of(request: &str) -> Option<String> {
    let line = request.lines().next()?;
    let mut parts = line.split(' ');
    let method = parts.next()?;
    let authority = parts.next()?;
    if !method.eq_ignore_ascii_case("CONNECT") || authority.is_empty() {
        return None;
    }
    Some(authority.to_owned())
}

/// Split a `host:port` the way the rows write it.
fn split_target(target: &str) -> (String, u16) {
    match target.rsplit_once(':') {
        Some((host, port)) => {
            let port = port.parse::<u16>().unwrap_or_default();
            (
                host.trim_start_matches('[')
                    .trim_end_matches(']')
                    .to_owned(),
                port,
            )
        }
        None => (target.to_owned(), 443),
    }
}

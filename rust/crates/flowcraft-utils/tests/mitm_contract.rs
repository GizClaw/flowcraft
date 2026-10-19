//! Replays the committed MITM contract against the port.
//!
//! `rust/conformance/mitm-contract.json` is dumped from core by
//! `tools/conformance-oracle -mitm-contract` and checked in; the gate fails if
//! regenerating it produces anything else. This test is the other half of the
//! handshake table -- the layer that needs no engine:
//!
//!   - `alpn`, whose rows are one handshake each, replayed against
//!     [`Ca::server_config`] over two in-memory `rustls` connections;
//!   - `tls-1-1-refused`, whose client is not a client but the bytes of a hello
//!     naming TLS 1.1, because neither stack can be asked to offer one.
//!
//! The decrypted layer -- `sessions` and `ambient` -- is replayed in
//! `tests/mitm_sessions.rs`, where the engine is.
//!
//! What the handshake rows pin, and why each is worth a row:
//!
//!   - the **server's** preference decides, not the client's, so a port that
//!     passes the client's order through negotiates h1 where core negotiates h2;
//!   - no ALPN offer is a successful handshake with no protocol, not a refusal;
//!   - TLS 1.2 is at the floor and TLS 1.1 is under it.

use std::sync::Arc;

use flowcraft_utils::net::mitm::ca::Ca;
use rustls::pki_types::ServerName;
use rustls::{
    ClientConfig, ClientConnection, ProtocolVersion, RootCertStore, ServerConfig, ServerConnection,
};
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../conformance/mitm-contract.json");

/// Core's `tls.Server` config: both protocols offered, TLS 1.2 at the floor.
const SERVER_ALPN: &[&[u8]] = &[b"h2", b"http/1.1"];

/// The name every row's tunnel is opened for.
const SERVER_NAME: &str = "example.test";

#[test]
fn the_handshake_contract_replays() {
    let document = contract();
    let ca = ca();
    let mut failures = Vec::new();

    for row in rows(&document, "alpn") {
        let name = string(row, "name");
        let outcome = match row.get("hello").and_then(Value::as_str) {
            Some(hello) => match decode_hello(hello) {
                Ok(bytes) => replay_hello(&ca, &bytes),
                Err(err) => {
                    failures.push(format!("{name}: {err}"));
                    continue;
                }
            },
            None => {
                let offered: Vec<Vec<u8>> = list(row, "offered")
                    .iter()
                    .map(|protocol| protocol.as_bytes().to_vec())
                    .collect();
                let max = match row.get("max_version").and_then(Value::as_str) {
                    Some(version) => match version_of(version) {
                        Some(version) => Some(version),
                        None => {
                            failures.push(format!("{name}: no client can be pinned to {version}"));
                            continue;
                        }
                    },
                    None => None,
                };
                replay_client(&ca, &offered, max)
            }
        };

        let want_ok = row.get("ok").and_then(Value::as_bool).unwrap_or(false);
        if outcome.ok != want_ok {
            let detail = match &outcome.refusal {
                Some(refusal) => format!(" ({refusal})"),
                None => String::new(),
            };
            failures.push(format!(
                "{name}: core {} the handshake and the port {}{detail}",
                if want_ok { "completed" } else { "refused" },
                if outcome.ok { "completed" } else { "refused" },
            ));
            continue;
        }
        if !want_ok {
            continue;
        }
        let want_negotiated = row.get("negotiated").and_then(Value::as_str).unwrap_or("");
        if outcome.negotiated != want_negotiated {
            failures.push(format!(
                "{name}: core negotiated {want_negotiated:?} and the port negotiated {:?}",
                outcome.negotiated
            ));
        }
        let want_version = row.get("tls_version").and_then(Value::as_str).unwrap_or("");
        if outcome.version != want_version {
            failures.push(format!(
                "{name}: core spoke {want_version:?} and the port spoke {:?}",
                outcome.version
            ));
        }
    }

    if !failures.is_empty() {
        panic!(
            "{} of the committed handshake rows do not replay:\n{}",
            failures.len(),
            failures.join("\n")
        );
    }
}

// ------------------------------------------------------------------- the state

/// The port's verdict on one handshake.
struct Outcome {
    ok: bool,
    negotiated: String,
    version: String,
    /// Why it was refused, when the port refused it.
    refusal: Option<String>,
}

/// Run a real client against the port's server config: the same client core's
/// rows use, with the ALPN offer and the version cap the row names.
fn replay_client(ca: &Ca, offered: &[Vec<u8>], max: Option<ProtocolVersion>) -> Outcome {
    let mut server = match server(ca) {
        Ok(server) => server,
        Err(err) => return refused(err),
    };
    let mut client = match client(ca, offered, max) {
        Ok(client) => client,
        Err(err) => return refused(err),
    };
    match pump(&mut server, &mut client) {
        Ok(()) => {}
        Err(err) => return refused(err),
    }
    Outcome {
        ok: true,
        negotiated: server
            .alpn_protocol()
            .map(|protocol| String::from_utf8_lossy(protocol).into_owned())
            .unwrap_or_default(),
        version: version_name(server.protocol_version()),
        refusal: None,
    }
}

/// Hand the port's server the bytes of a hello and see whether it completes.
/// It must not: the hello names TLS 1.1 and the floor is 1.2.
fn replay_hello(ca: &Ca, hello: &[u8]) -> Outcome {
    let mut server = match server(ca) {
        Ok(server) => server,
        Err(err) => return refused(err),
    };
    let mut read = 0;
    while read < hello.len() {
        match server.read_tls(&mut &hello[read..]) {
            Ok(0) => return refused("the server stopped reading the hello".to_owned()),
            Ok(count) => read += count,
            Err(err) => return refused(err.to_string()),
        }
    }
    match server.process_new_packets() {
        // A protocol alert is the expected answer and is not compared: two
        // stacks do not agree on which one, and which one it is is not the
        // contract -- that the handshake stops is.
        Err(err) => Outcome {
            ok: false,
            negotiated: String::new(),
            version: String::new(),
            refusal: Some(err.to_string()),
        },
        Ok(_) => Outcome {
            ok: true,
            negotiated: server
                .alpn_protocol()
                .map(|protocol| String::from_utf8_lossy(protocol).into_owned())
                .unwrap_or_default(),
            version: version_name(server.protocol_version()),
            refusal: None,
        },
    }
}

fn refused(reason: String) -> Outcome {
    Outcome {
        ok: false,
        negotiated: String::new(),
        version: String::new(),
        refusal: Some(reason),
    }
}

/// Drive both sides until neither wants to write. In memory on purpose: no
/// socket, no port, nothing to wait for.
fn pump(server: &mut ServerConnection, client: &mut ClientConnection) -> Result<(), String> {
    for _ in 0..8 {
        let to_server = client_wire(client)?;
        server_read(server, &to_server)?;
        let to_client = server_wire(server)?;
        client_read(client, &to_client)?;
        if !client.is_handshaking() && !server.is_handshaking() {
            return Ok(());
        }
    }
    Err("the handshake did not finish".to_owned())
}

fn client_wire(client: &mut ClientConnection) -> Result<Vec<u8>, String> {
    let mut wire = Vec::new();
    while client.wants_write() {
        client.write_tls(&mut wire).map_err(|err| err.to_string())?;
    }
    Ok(wire)
}

fn server_wire(server: &mut ServerConnection) -> Result<Vec<u8>, String> {
    let mut wire = Vec::new();
    while server.wants_write() {
        server.write_tls(&mut wire).map_err(|err| err.to_string())?;
    }
    Ok(wire)
}

fn server_read(server: &mut ServerConnection, wire: &[u8]) -> Result<(), String> {
    let mut read = 0;
    while read < wire.len() {
        read += server
            .read_tls(&mut &wire[read..])
            .map_err(|err| err.to_string())?;
    }
    server
        .process_new_packets()
        .map(|_| ())
        .map_err(|err| err.to_string())
}

fn client_read(client: &mut ClientConnection, wire: &[u8]) -> Result<(), String> {
    let mut read = 0;
    while read < wire.len() {
        read += client
            .read_tls(&mut &wire[read..])
            .map_err(|err| err.to_string())?;
    }
    client
        .process_new_packets()
        .map(|_| ())
        .map_err(|err| err.to_string())
}

fn server(ca: &Ca) -> Result<ServerConnection, String> {
    let config: Arc<ServerConfig> = ca
        .server_config(SERVER_NAME, SERVER_ALPN)
        .map_err(|err| err.to_string())?;
    ServerConnection::new(config).map_err(|err| err.to_string())
}

fn client(
    ca: &Ca,
    offered: &[Vec<u8>],
    max: Option<ProtocolVersion>,
) -> Result<ClientConnection, String> {
    let mut roots = RootCertStore::empty();
    roots
        .add(ca.root().clone())
        .map_err(|err| err.to_string())?;
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let builder = ClientConfig::builder_with_provider(provider);
    let builder = match max {
        Some(ProtocolVersion::TLSv1_2) => {
            builder.with_protocol_versions(&[&rustls::version::TLS12])
        }
        Some(ProtocolVersion::TLSv1_3) => {
            builder.with_protocol_versions(&[&rustls::version::TLS13])
        }
        Some(other) => return Err(format!("no client can be pinned to {other:?}")),
        None => builder.with_safe_default_protocol_versions(),
    }
    .map_err(|err| err.to_string())?;
    let mut config = builder.with_root_certificates(roots).with_no_client_auth();
    config.alpn_protocols = offered.to_vec();
    let name = ServerName::try_from(SERVER_NAME.to_owned()).map_err(|err| err.to_string())?;
    ClientConnection::new(Arc::new(config), name).map_err(|err| err.to_string())
}

fn ca() -> Ca {
    match Ca::new() {
        Ok(ca) => ca,
        Err(err) => panic!("the authority should mint: {err}"),
    }
}

// ------------------------------------------------------------------ the shapes

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

fn list(row: &Value, field: &str) -> Vec<String> {
    row.get(field)
        .and_then(Value::as_array)
        .map(|values| {
            values
                .iter()
                .filter_map(Value::as_str)
                .map(str::to_owned)
                .collect()
        })
        .unwrap_or_default()
}

fn decode_hello(hello: &str) -> Result<Vec<u8>, String> {
    let mut bytes = Vec::with_capacity(hello.len() / 2);
    let digits = hello.as_bytes();
    if !digits.len().is_multiple_of(2) {
        return Err("the hello is not an even number of hex digits".to_owned());
    }
    for pair in digits.chunks(2) {
        let text = std::str::from_utf8(pair).map_err(|err| err.to_string())?;
        bytes.push(u8::from_str_radix(text, 16).map_err(|err| err.to_string())?);
    }
    Ok(bytes)
}

fn version_of(name: &str) -> Option<ProtocolVersion> {
    match name {
        "TLS 1.2" => Some(ProtocolVersion::TLSv1_2),
        "TLS 1.3" => Some(ProtocolVersion::TLSv1_3),
        _ => None,
    }
}

fn version_name(version: Option<ProtocolVersion>) -> String {
    match version {
        Some(ProtocolVersion::TLSv1_2) => "TLS 1.2".to_owned(),
        Some(ProtocolVersion::TLSv1_3) => "TLS 1.3".to_owned(),
        Some(other) => format!("{other:?}"),
        None => String::new(),
    }
}

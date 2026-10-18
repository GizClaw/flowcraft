//! Replays the committed proxy contract against the port.
//!
//! `rust/conformance/proxy-contract.json` is dumped from core by
//! `tools/conformance-oracle -proxy-contract` and checked in; the gate fails if
//! regenerating it produces anything else. This test is the other half: every
//! URL is parsed, every policy validated, every destination decided, and every
//! MITM selection asked, and the answers are compared with core's.
//!
//! The rows exist because each of these four is a place a port reads right and
//! answers wrong: `url.Parse`'s refusals are part of validation and their
//! wording is what an operator sees; `Validate`'s order decides *which* of two
//! mistakes is reported; `splitHostPort` has two fallbacks that turn a refused
//! split into one trimmed host; and `Decide` has four branches (address,
//! hostname, address-pinned, mode default) that a plausible port collapses into
//! two.

use flowcraft_utils::net::mitm::Selection;
use flowcraft_utils::net::policy::{MitmPolicy, NetAction, NetMode, NetPolicy, NetRule};
use flowcraft_utils::net::proxy::{Proxy, ProxyConfig};
use flowcraft_utils::net::url;
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../conformance/proxy-contract.json");

#[test]
fn the_proxy_contract_replays() {
    let document = contract();
    let mut failures = Vec::new();

    replay_urls(&document, &mut failures);
    replay_validations(&document, &mut failures);
    replay_decisions(&document, &mut failures);
    replay_mitm(&document, &mut failures);

    if !failures.is_empty() {
        panic!(
            "{} of the committed samples do not replay:\n{}",
            failures.len(),
            failures.join("\n")
        );
    }
}

/// The contract is a set of tables, and an empty one would replay perfectly.
#[test]
fn the_contract_still_covers_every_table() {
    let document = contract();
    for (table, least) in [
        ("urls", 40),
        ("validations", 20),
        ("decisions", 25),
        ("mitm", 6),
    ] {
        let count = rows(&document, table).len();
        assert!(
            count >= least,
            "{table} has {count} rows, expected at least {least}"
        );
    }
}

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
    match row[field].as_bool() {
        Some(value) => value,
        None => panic!("{field} is not a bool in {row}"),
    }
}

fn number(row: &Value, field: &str) -> i64 {
    match row[field].as_i64() {
        Some(value) => value,
        None => panic!("{field} is not a number in {row}"),
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

fn mode(row: &Value) -> NetMode {
    match text(row, "mode") {
        "default" => NetMode::Default,
        "deny_all" => NetMode::DenyAll,
        "allow_list" => NetMode::AllowList,
        "proxy" => NetMode::Proxy,
        other => panic!("unknown mode {other:?} in {row}"),
    }
}

fn action(row: &Value) -> NetAction {
    match text(row, "action") {
        "allow" => NetAction::Allow,
        "deny" => NetAction::Deny,
        other => panic!("unknown action {other:?} in {row}"),
    }
}

fn rules(row: &Value) -> Vec<NetRule> {
    match row["rules"].as_array() {
        Some(values) => values
            .iter()
            .map(|rule| NetRule {
                action: action(rule),
                host: text(rule, "host").to_owned(),
                port: i32::try_from(number(rule, "port")).unwrap_or_default(),
                reason: optional(rule, "reason").unwrap_or_default().to_owned(),
            })
            .collect(),
        None => Vec::new(),
    }
}

fn mitm_policy(row: &Value) -> Option<MitmPolicy> {
    let policy = row.get("mitm")?.as_object()?;
    Some(MitmPolicy {
        enabled: policy["enabled"].as_bool().unwrap_or_default(),
        inspect_bodies: policy["inspect_bodies"].as_bool().unwrap_or_default(),
        max_body_bytes: policy["max_body_bytes"].as_i64().unwrap_or_default(),
        hosts: strings(&Value::Object(policy.clone()), "hosts"),
        exclude_hosts: strings(&Value::Object(policy.clone()), "exclude_hosts"),
    })
}

fn replay_urls(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "urls") {
        let raw = text(row, "raw");
        let expected_error = optional(row, "error");
        match (url::parse(raw), expected_error) {
            (Err(err), Some(expected)) => {
                if err.to_string() != expected {
                    failures.push(format!(
                        "parse({raw:?}): port says {err:?}, core {expected:?}"
                    ));
                }
            }
            (Ok(parsed), None) => {
                let expected = [
                    ("scheme", parsed.scheme.as_str()),
                    ("host", parsed.host.as_str()),
                    ("path", parsed.path.as_str()),
                    ("raw_query", parsed.raw_query.as_str()),
                    ("fragment", parsed.fragment.as_str()),
                    ("opaque", parsed.opaque.as_str()),
                ];
                for (field, actual) in expected {
                    let want = optional(row, field).unwrap_or("");
                    if actual != want {
                        failures.push(format!(
                            "parse({raw:?}): {field} is {actual:?}, core {want:?}"
                        ));
                    }
                }
                match &parsed.user {
                    Some(user) => {
                        if !flag(row, "has_user") {
                            failures
                                .push(format!("parse({raw:?}): port has userinfo, core has none"));
                        }
                        if user.username != optional(row, "user").unwrap_or("") {
                            failures.push(format!(
                                "parse({raw:?}): user is {:?}, core {:?}",
                                user.username,
                                optional(row, "user")
                            ));
                        }
                        if user.password.is_some() != flag(row, "has_password") {
                            failures.push(format!(
                                "parse({raw:?}): has_password is {}, core {}",
                                user.password.is_some(),
                                flag(row, "has_password")
                            ));
                        }
                        if user.password.as_deref().unwrap_or("")
                            != optional(row, "password").unwrap_or("")
                        {
                            failures.push(format!(
                                "parse({raw:?}): password is {:?}, core {:?}",
                                user.password,
                                optional(row, "password")
                            ));
                        }
                    }
                    None => {
                        if flag(row, "has_user") {
                            failures.push(format!("parse({raw:?}): port lost the userinfo"));
                        }
                    }
                }
            }
            (Err(err), None) => {
                failures.push(format!(
                    "parse({raw:?}): port refuses it ({err}), core accepts"
                ));
            }
            (Ok(_), Some(expected)) => {
                failures.push(format!(
                    "parse({raw:?}): port accepts it, core refuses with {expected:?}"
                ));
            }
        }
    }
}

fn replay_validations(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "validations") {
        let name = text(row, "name");
        let policy = NetPolicy {
            mode: mode(row),
            allow_hosts: strings(row, "allow_hosts"),
            rules: rules(row),
            proxy: optional(row, "proxy").unwrap_or_default().to_owned(),
            unix_sockets: strings(row, "unix_sockets"),
            mitm: mitm_policy(row),
        };
        match (policy.validate(), optional(row, "error")) {
            (Err(err), Some(expected)) => {
                if err.to_string() != expected {
                    failures.push(format!(
                        "validation {name:?}: port says {err:?}, core {expected:?}"
                    ));
                }
            }
            (Ok(()), None) => {}
            (Err(err), None) => {
                failures.push(format!(
                    "validation {name:?}: port refuses it ({err}), core accepts"
                ));
            }
            (Ok(()), Some(expected)) => {
                failures.push(format!(
                    "validation {name:?}: port accepts it, core refuses with {expected:?}"
                ));
            }
        }
    }
}

fn replay_decisions(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "decisions") {
        let name = text(row, "name");
        let config = ProxyConfig {
            mode: mode(row),
            allow_hosts: strings(row, "allow_hosts"),
            rules: rules(row),
            upstream: optional(row, "upstream").unwrap_or_default().to_owned(),
            mitm: None,
        };
        // Every decision row is answerable without the resolver: a name plus an
        // IP rule is the one path that resolves, and those rows are not in the
        // contract (see the oracle's note).
        let proxy = Proxy::new(&config);
        let verdict = proxy.decide(
            text(row, "request"),
            i32::try_from(number(row, "default_port")).unwrap_or_default(),
        );
        let record = &verdict.record;
        let expected_error = optional(row, "error");
        match (&verdict.error, expected_error) {
            (Some(err), Some(expected)) => {
                if err.to_string() != expected {
                    failures.push(format!(
                        "decision {name:?}: port says {err:?}, core {expected:?}"
                    ));
                }
            }
            (None, None) => {}
            (Some(err), None) => {
                failures.push(format!(
                    "decision {name:?}: port errors ({err}), core does not"
                ));
            }
            (None, Some(expected)) => {
                failures.push(format!(
                    "decision {name:?}: port decides, core errors with {expected:?}"
                ));
            }
        }
        let expected = [
            ("host", record.host.as_str()),
            ("dial_host", record.dial_host.as_str()),
            ("rule", record.rule.as_str()),
            ("reason", record.reason.as_str()),
            ("action", record.action.as_str()),
        ];
        for (field, actual) in expected {
            let want = optional(row, field).unwrap_or("");
            if actual != want {
                failures.push(format!(
                    "decision {name:?}: {field} is {actual:?}, core {want:?}"
                ));
            }
        }
        if i64::from(record.port) != number(row, "port") {
            failures.push(format!(
                "decision {name:?}: port is {}, core {}",
                record.port,
                number(row, "port")
            ));
        }
    }
}

fn replay_mitm(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "mitm") {
        let name = text(row, "name");
        let policy = MitmPolicy {
            enabled: true,
            inspect_bodies: false,
            max_body_bytes: 0,
            hosts: strings(row, "hosts"),
            exclude_hosts: strings(row, "exclude_hosts"),
        };
        match (Selection::new(&policy), optional(row, "error")) {
            (Err(err), Some(expected)) => {
                if err.to_string() != expected {
                    failures.push(format!(
                        "mitm {name:?}: port says {err:?}, core {expected:?}"
                    ));
                }
            }
            (Ok(selection), None) => {
                let queries = strings(row, "queries");
                let answers = match row["answers"].as_array() {
                    Some(answers) => answers,
                    None => {
                        failures.push(format!("mitm {name:?}: no answers"));
                        continue;
                    }
                };
                if queries.len() != answers.len() {
                    failures.push(format!(
                        "mitm {name:?}: {} queries but {} answers",
                        queries.len(),
                        answers.len()
                    ));
                    continue;
                }
                for (query, answer) in queries.iter().zip(answers) {
                    let actual = selection.should_mitm(query);
                    if Some(actual) != answer.as_bool() {
                        failures.push(format!(
                            "mitm {name:?}: {query:?} is {actual}, core {answer}"
                        ));
                    }
                }
            }
            (Err(err), None) => {
                failures.push(format!(
                    "mitm {name:?}: port refuses it ({err}), core accepts"
                ));
            }
            (Ok(_), Some(expected)) => {
                failures.push(format!(
                    "mitm {name:?}: port accepts it, core refuses with {expected:?}"
                ));
            }
        }
    }
}

//! Replays the committed net contract against the port.
//!
//! `rust/conformance/net-contract.json` is dumped from core by
//! `tools/conformance-oracle -net-contract` and checked in; the gate fails if
//! regenerating it produces anything else. This test is the other half: every
//! pattern is asked about every host and every address in the dump, and every
//! policy is asked about the destinations it lists.
//!
//! The rows exist because the pattern grammar reads as if it had two cases and
//! has four, each with a spelling that a plausible port gets wrong: a bare
//! domain is domain-*and-descendants*, `*.example.com` is not, `1.2.3` is a
//! name rather than an address, `10.0.0.0/33` is a name rather than a range,
//! `[::1]:8443` is an address and `[::1]` is a name, and normalization runs on
//! both sides of the comparison but IDNA only on the non-ASCII side.

use flowcraft_utils::net::addr::Addr;
use flowcraft_utils::net::host;
use flowcraft_utils::net::{Matcher, NetAction, NetPolicy, NetRule, Set};
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../conformance/net-contract.json");

#[test]
fn the_net_contract_replays() {
    let document = contract();
    let mut failures = Vec::new();

    replay_patterns(&document, &mut failures);
    replay_sets(&document, &mut failures);
    replay_matchers(&document, &mut failures);

    if !failures.is_empty() {
        panic!(
            "{} of the committed samples do not replay:\n{}",
            failures.len(),
            failures.join("\n")
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

fn host_queries(document: &Value) -> Vec<String> {
    rows(document, "host_queries")
        .iter()
        .map(|value| match value.as_str() {
            Some(host) => host.to_owned(),
            None => panic!("host_queries holds a non-string: {value}"),
        })
        .collect()
}

fn ip_queries(document: &Value) -> Vec<(String, Addr)> {
    rows(document, "ip_queries")
        .iter()
        .map(|value| {
            let raw = match value.as_str() {
                Some(raw) => raw,
                None => panic!("ip_queries holds a non-string: {value}"),
            };
            match Addr::parse(raw) {
                Some(addr) => (raw.to_owned(), addr),
                None => panic!("the port cannot parse the committed query {raw:?}"),
            }
        })
        .collect()
}

/// Compare a row's `match`/`match_ip` vectors against what the port answers.
fn compare(
    where_: &str,
    row: &Value,
    hosts: &[String],
    ips: &[(String, Addr)],
    matches: impl Fn(&str) -> bool,
    matches_ip: impl Fn(&Addr) -> bool,
    failures: &mut Vec<String>,
) {
    let expected = match row["match"].as_array() {
        Some(values) => values,
        None => {
            failures.push(format!("{where_}: no match vector"));
            return;
        }
    };
    if expected.len() != hosts.len() {
        failures.push(format!(
            "{where_}: {} match entries for {} hosts",
            expected.len(),
            hosts.len()
        ));
        return;
    }
    for (host, expected) in hosts.iter().zip(expected) {
        let expected = expected.as_bool().unwrap_or_default();
        let got = matches(host);
        if got != expected {
            failures.push(format!("{where_}: host {host:?} -> {got}, core {expected}"));
        }
    }

    let expected = match row["match_ip"].as_array() {
        Some(values) => values,
        None => {
            failures.push(format!("{where_}: no match_ip vector"));
            return;
        }
    };
    if expected.len() != ips.len() {
        failures.push(format!(
            "{where_}: {} match_ip entries for {} addresses",
            expected.len(),
            ips.len()
        ));
        return;
    }
    for ((raw, addr), expected) in ips.iter().zip(expected) {
        let expected = expected.as_bool().unwrap_or_default();
        let got = matches_ip(addr);
        if got != expected {
            failures.push(format!("{where_}: ip {raw:?} -> {got}, core {expected}"));
        }
    }
}

fn replay_patterns(document: &Value, failures: &mut Vec<String>) {
    let hosts = host_queries(document);
    let ips = ip_queries(document);
    for row in rows(document, "patterns") {
        let raw = text(row, "raw");
        let where_ = format!("compile({raw:?})");
        let expected_error = row["error"].as_str();
        match (host::compile(raw), expected_error) {
            (Err(err), Some(expected)) => {
                if err.to_string() != expected {
                    failures.push(format!("{where_}: port says {err:?}, core {expected:?}"));
                }
            }
            (Ok(pattern), None) => {
                let expected = flag(row, "is_ip");
                if pattern.is_ip() != expected {
                    failures.push(format!(
                        "{where_}: is_ip -> {}, core {expected}",
                        pattern.is_ip()
                    ));
                }
                compare(
                    &where_,
                    row,
                    &hosts,
                    &ips,
                    |host| pattern.matches_host(host),
                    |addr| pattern.matches_ip(addr),
                    failures,
                );
            }
            (Err(err), None) => {
                failures.push(format!("{where_}: port refuses it ({err}), core accepts"));
            }
            (Ok(_), Some(expected)) => {
                failures.push(format!(
                    "{where_}: port accepts it, core refuses with {expected:?}"
                ));
            }
        }
    }
}

fn replay_sets(document: &Value, failures: &mut Vec<String>) {
    let hosts = host_queries(document);
    let ips = ip_queries(document);
    for row in rows(document, "sets") {
        let entries: Vec<String> = match row["hosts"].as_array() {
            Some(values) => values
                .iter()
                .map(|value| value.as_str().unwrap_or_default().to_owned())
                .collect(),
            None => {
                failures.push("a set row has no hosts".to_owned());
                continue;
            }
        };
        let where_ = format!("Set::new({entries:?})");
        let expected_error = row["error"].as_str();
        match (Set::new(&entries), expected_error) {
            (Err(err), Some(expected)) => {
                if err.to_string() != expected {
                    failures.push(format!("{where_}: port says {err:?}, core {expected:?}"));
                }
            }
            (Ok(set), None) => {
                let expected = flag(row, "empty");
                if set.is_empty() != expected {
                    failures.push(format!(
                        "{where_}: empty -> {}, core {expected}",
                        set.is_empty()
                    ));
                }
                compare(
                    &where_,
                    row,
                    &hosts,
                    &ips,
                    |host| set.matches_host(host),
                    |addr| set.matches_ip(addr),
                    failures,
                );
            }
            (Err(err), None) => {
                failures.push(format!("{where_}: port refuses it ({err}), core accepts"));
            }
            (Ok(_), Some(expected)) => {
                failures.push(format!(
                    "{where_}: port accepts it, core refuses with {expected:?}"
                ));
            }
        }
    }
}

fn replay_matchers(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "matchers") {
        let name = text(row, "name");
        let policy = policy(row);
        let where_ = format!("matcher {name:?}");
        let expected_error = row["error"].as_str();
        let matcher = match (Matcher::new(&policy), expected_error) {
            (Err(err), Some(expected)) => {
                if err.to_string() != expected {
                    failures.push(format!("{where_}: port says {err:?}, core {expected:?}"));
                }
                continue;
            }
            (Ok(matcher), None) => matcher,
            (Err(err), None) => {
                failures.push(format!("{where_}: port refuses it ({err}), core accepts"));
                continue;
            }
            (Ok(_), Some(expected)) => {
                failures.push(format!(
                    "{where_}: port accepts it, core refuses with {expected:?}"
                ));
                continue;
            }
        };

        let expected = flag(row, "has_ip_rules");
        if matcher.has_ip_rules() != expected {
            failures.push(format!(
                "{where_}: has_ip_rules -> {}, core {expected}",
                matcher.has_ip_rules()
            ));
        }

        if let Some(queries) = row["hosts"].as_array() {
            for query in queries {
                let host = text(query, "host");
                let port = number(query, "port");
                let port = match i32::try_from(port) {
                    Ok(port) => port,
                    Err(_) => {
                        failures.push(format!("{where_}: port {port} is not an i32"));
                        continue;
                    }
                };
                let decision = matcher.match_host(host, port);
                compare_decision(where_.as_str(), query, &decision, failures);
            }
        }

        if let Some(queries) = row["ips"].as_array() {
            for query in queries {
                let raw = text(query, "ip");
                let addr = match Addr::parse(raw) {
                    Some(addr) => addr,
                    None => {
                        failures.push(format!("{where_}: port cannot parse {raw:?}"));
                        continue;
                    }
                };
                let port = number(query, "port");
                let port = match i32::try_from(port) {
                    Ok(port) => port,
                    Err(_) => {
                        failures.push(format!("{where_}: port {port} is not an i32"));
                        continue;
                    }
                };
                let decision = matcher.match_ip(&addr, port);
                compare_decision(where_.as_str(), query, &decision, failures);
            }
        }
    }
}

fn policy(row: &Value) -> NetPolicy {
    let mut policy = NetPolicy::default();
    if let Some(entries) = row["allow_hosts"].as_array() {
        policy.allow_hosts = entries
            .iter()
            .map(|value| value.as_str().unwrap_or_default().to_owned())
            .collect();
    }
    if let Some(rules) = row["rules"].as_array() {
        policy.rules = rules
            .iter()
            .map(|rule| NetRule {
                action: match text(rule, "action") {
                    "deny" => NetAction::Deny,
                    "allow" => NetAction::Allow,
                    other => panic!("unknown action {other:?}"),
                },
                host: text(rule, "host").to_owned(),
                port: i32::try_from(number(rule, "port"))
                    .unwrap_or_else(|_| panic!("port out of range in {rule}")),
                reason: rule["reason"].as_str().unwrap_or_default().to_owned(),
            })
            .collect();
    }
    policy
}

fn compare_decision(
    where_: &str,
    query: &Value,
    decision: &flowcraft_utils::net::Decision,
    failures: &mut Vec<String>,
) {
    let subject = match query["ip"].as_str() {
        Some(ip) => format!("ip {ip:?}:{}", number(query, "port")),
        None => format!("host {:?}:{}", text(query, "host"), number(query, "port")),
    };
    let expected_action = text(query, "action");
    if decision.action.as_str() != expected_action {
        failures.push(format!(
            "{where_}: {subject} -> {}, core {expected_action}",
            decision.action
        ));
    }
    let expected_rule = text(query, "rule");
    if decision.rule != expected_rule {
        failures.push(format!(
            "{where_}: {subject} -> rule {:?}, core {expected_rule:?}",
            decision.rule
        ));
    }
    let expected_reason = query["reason"].as_str().unwrap_or_default();
    if decision.reason != expected_reason {
        failures.push(format!(
            "{where_}: {subject} -> reason {:?}, core {expected_reason:?}",
            decision.reason
        ));
    }
    let expected_matched = flag(query, "matched");
    if decision.matched != expected_matched {
        failures.push(format!(
            "{where_}: {subject} -> matched {}, core {expected_matched}",
            decision.matched
        ));
    }
}

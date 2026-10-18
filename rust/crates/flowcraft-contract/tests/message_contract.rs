//! Replays the committed message contract against the port.
//!
//! `rust/conformance/message-contract.json` is dumped from core by
//! `tools/conformance-oracle -message-contract` and checked in; the gate fails
//! if regenerating it produces anything else. This test is the other half: it
//! rebuilds every sample -- a constructor call, a wire document, a format, a
//! message, a tool DTO, a schema builder call -- and holds the port's answer to
//! core's, sentence by sentence. Six tables: the media DTOs, the audio formats,
//! the parts, the messages, the tools and the schemas.
//!
//! # The sentence that is not compared
//!
//! Some refusals in the file are *by `encoding/json`* rather than by core: a
//! wrong JSON type, a bad base64 payload, a second document after the first, a
//! marshal callback that failed. The oracle computes which ones (`textFrom`),
//! and the rows say so (`text_from`); this test holds those to being refused at
//! all, because a hand-written decoder does not reproduce another library's
//! syntax diagnostics. Everything else -- core's own sentences, and the
//! `json: unknown field %q` shape, which is small enough to reproduce -- is
//! compared exactly.
//!
//! # Three refusals the port moves one stage earlier
//!
//! The port's types have no invalid state, so three documents core decodes and
//! refuses later are refused here at decode, with core's own sentence: a
//! message role outside the four, a part whose `source` is absent, and an audio
//! format whose `encoding` is outside the table. Same refusal, same words,
//! different moment. The corpus samples an absent source through its `null`
//! spelling (which core refuses at decode too) and does not sample the other
//! two at all; `the_earlier_refusals` below is where they are pinned.

use flowcraft_contract::message::parts::Part;
use flowcraft_contract::message::schema::{
    ToolPropertyDef, define_schema, tool_array_property, tool_enum_property, tool_object_property,
    tool_property, tool_property_with_default, tool_string_map_property,
};
use flowcraft_contract::message::{Content, Message, ToolCall, ToolDefinition, ToolResult};
use serde_json::Map;

use base64::Engine as _;
use flowcraft_contract::message::media::{
    AudioEncoding, AudioFormat, AudioSource, ImageSource, VideoSource,
};
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../conformance/message-contract.json");

/// One sample's answer: the normalized type, the base type, and the wire form.
type Answer = Result<(String, String, String), String>;

#[test]
fn the_message_contract_replays() {
    let document = contract();
    // A contract that lost its rows would still replay: everything in it
    // would pass. These floors are the same numbers the dump has today.
    for (table, floor) in [
        ("media_sources", 50),
        ("audio_formats", 15),
        ("parts", 45),
        ("messages", 15),
        ("tools", 12),
        ("schemas", 6),
    ] {
        let len = rows(&document, table).len();
        if len < floor {
            panic!("the contract has {len} rows in {table}, fewer than the {floor} it used to");
        }
    }
    let mut failures = Vec::new();
    for row in rows(&document, "media_sources") {
        replay_media(row, &mut failures);
    }
    for row in rows(&document, "audio_formats") {
        replay_format(row, &mut failures);
    }
    for row in rows(&document, "parts") {
        replay_part(row, &mut failures);
    }
    for row in rows(&document, "messages") {
        replay_message(row, &mut failures);
    }
    for row in rows(&document, "tools") {
        replay_tool(row, &mut failures);
    }
    for row in rows(&document, "schemas") {
        replay_schema(row, &mut failures);
    }
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

fn replay_media(row: &Value, failures: &mut Vec<String>) {
    let target = str_of(row, "target");
    let op = str_of(row, "op");
    let media_type = str_of(row, "media_type");
    let where_ = format!("{target}/{op}/{media_type}");

    let answer = port_media(target, op, media_type, row);

    match str_of(row, "outcome") {
        "ok" => {
            let expected = (
                str_of(row, "media_type_out").to_owned(),
                str_of(row, "base_media_type").to_owned(),
                str_of(row, "wire").to_owned(),
            );
            match answer {
                Ok(got) if got == expected => {}
                Ok(got) => failures.push(format!(
                    "{where_}: port answered {got:?}, core {expected:?}"
                )),
                Err(err) => failures.push(format!(
                    "{where_}: port refused with {err:?}, core accepted {}",
                    expected.2
                )),
            }
        }
        "error" => {
            let expected = str_of(row, "error");
            match answer {
                Ok(got) => failures.push(format!(
                    "{where_}: port accepted {got:?}, core refused with {expected:?}"
                )),
                // The decoder's own refusals are compared as refusals: the
                // port does not speak encoding/json (see the module docs).
                Err(_) if str_of(row, "text_from") == "encoding/json" => {}
                Err(err) if err == expected => {}
                Err(err) => {
                    failures.push(format!("{where_}: port said {err:?}, core {expected:?}"))
                }
            }
        }
        other => failures.push(format!("{where_}: unknown outcome {other:?}")),
    }
}

fn port_media(target: &str, op: &str, media_type: &str, row: &Value) -> Answer {
    match op {
        "new_inline" => {
            let bytes = match base64::engine::general_purpose::STANDARD.decode(str_of(row, "bytes"))
            {
                Ok(bytes) => bytes,
                Err(err) => return Err(format!("payload: {err}")),
            };
            match target {
                "image" => view_inline(&bytes, media_type),
                "audio" => view_audio_inline(&bytes, media_type),
                "video" => view_video_inline(&bytes, media_type),
                other => Err(format!("unknown target {other}")),
            }
        }
        "new_url" => {
            let url = str_of(row, "url");
            match target {
                "image" => view_image_url(url, media_type),
                "audio" => view_audio_url(url, media_type),
                "video" => view_video_url(url, media_type),
                other => Err(format!("unknown target {other}")),
            }
        }
        "from_wire" => {
            let document = str_of(row, "document");
            match target {
                "image" => view_image(ImageSource::from_wire_json(document)),
                "audio" => view_audio(AudioSource::from_wire_json(document)),
                "video" => view_video(VideoSource::from_wire_json(document)),
                other => Err(format!("unknown target {other}")),
            }
        }
        other => Err(format!("unknown op {other}")),
    }
}

fn view_image(source: Result<ImageSource, String>) -> Answer {
    let source = source?;
    let wire = source.to_wire_json()?;
    Ok((
        source.media_type().to_owned(),
        source.base_media_type().to_owned(),
        wire,
    ))
}

fn view_audio(source: Result<AudioSource, String>) -> Answer {
    let source = source?;
    let wire = source.to_wire_json()?;
    Ok((
        source.media_type().to_owned(),
        source.base_media_type().to_owned(),
        wire,
    ))
}

fn view_video(source: Result<VideoSource, String>) -> Answer {
    let source = source?;
    let wire = source.to_wire_json()?;
    Ok((
        source.media_type().to_owned(),
        source.base_media_type().to_owned(),
        wire,
    ))
}

fn view_inline(bytes: &[u8], media_type: &str) -> Answer {
    view_image(ImageSource::new_inline(bytes, media_type))
}

fn view_audio_inline(bytes: &[u8], media_type: &str) -> Answer {
    view_audio(AudioSource::new_inline(bytes, media_type))
}

fn view_video_inline(bytes: &[u8], media_type: &str) -> Answer {
    view_video(VideoSource::new_inline(bytes, media_type))
}

fn view_image_url(url: &str, media_type: &str) -> Answer {
    view_image(ImageSource::new_url(url, media_type))
}

fn view_audio_url(url: &str, media_type: &str) -> Answer {
    view_audio(AudioSource::new_url(url, media_type))
}

fn view_video_url(url: &str, media_type: &str) -> Answer {
    view_video(VideoSource::new_url(url, media_type))
}

fn replay_format(row: &Value, failures: &mut Vec<String>) {
    let name = str_of(row, "encoding");
    let mut where_ = format!("format/{name}");
    if let (Some(rate), Some(channels)) = (number(row, "sample_rate_hz"), number(row, "channels")) {
        where_ = format!("{where_}/{rate}/{channels}");
    }

    let encoding = match AudioEncoding::parse(name) {
        Some(encoding) => encoding,
        None => {
            failures.push(format!("{where_}: the port does not know this encoding"));
            return;
        }
    };
    if encoding.media_type() != str_of(row, "media_type") {
        failures.push(format!(
            "{where_}: port maps it to {:?}, core to {:?}",
            encoding.media_type(),
            str_of(row, "media_type")
        ));
    }
    let format = AudioFormat::new(
        encoding,
        number(row, "sample_rate_hz"),
        number(row, "channels"),
    );
    let answer = format.validate();
    match str_of(row, "outcome") {
        "ok" => {
            if let Err(err) = answer {
                failures.push(format!(
                    "{where_}: port refused with {err:?}, core accepted"
                ));
            }
        }
        _ => {
            let expected = str_of(row, "error");
            match answer {
                Ok(()) => failures.push(format!("{where_}: port accepted, core said {expected:?}")),
                Err(err) if err == expected => {}
                Err(err) => {
                    failures.push(format!("{where_}: port said {err:?}, core {expected:?}"))
                }
            }
        }
    }
}

fn str_of<'a>(row: &'a Value, field: &str) -> &'a str {
    row[field].as_str().unwrap_or_default()
}

fn number(row: &Value, field: &str) -> Option<i32> {
    row[field].as_i64().map(|value| value as i32)
}

/// One stage's answer against the row's: `ok`/`error`, a sentence, and whether
/// that sentence is core's to compare (see the module docs).
fn stage(
    got: Result<(), String>,
    expected_ok: bool,
    expected: &str,
    text_from: &str,
    what: &str,
    failures: &mut Vec<String>,
) {
    match (expected_ok, got) {
        (true, Ok(())) => {}
        (true, Err(err)) => {
            failures.push(format!("{what}: port refused with {err:?}, core accepted"))
        }
        (false, Ok(())) => {
            failures.push(format!(
                "{what}: port accepted, core refused with {expected:?}"
            ));
        }
        (false, Err(_)) if text_from == "encoding/json" => {}
        (false, Err(err)) if err == expected => {}
        (false, Err(err)) => failures.push(format!("{what}: port said {err:?}, core {expected:?}")),
    }
}

/// The write stage, where the answer is the wire text rather than a sentence.
fn written(got: Result<String, String>, row: &Value, what: &str, failures: &mut Vec<String>) {
    let text_from = text_of(row, "wire_text_from");
    match (row.get("wire"), got) {
        (Some(expected_value), Ok(json)) => {
            let expected = expected_value.as_str().unwrap_or_default();
            if json != expected {
                failures.push(format!("{what}: port wrote {json:?}, core {expected:?}"));
            }
        }
        (Some(_), Err(err)) => {
            failures.push(format!(
                "{what}: port refused to write with {err:?}, core wrote"
            ));
        }
        (None, Ok(json)) => failures.push(format!(
            "{what}: port wrote {json:?}, core refused with {:?}",
            text_of(row, "wire_error")
        )),
        (None, Err(_)) if text_from == "encoding/json" => {}
        (None, Err(err)) if err == text_of(row, "wire_error") => {}
        (None, Err(err)) => failures.push(format!(
            "{what}: port refused with {err:?}, core {:?}",
            text_of(row, "wire_error")
        )),
    }
}

fn replay_part(row: &Value, failures: &mut Vec<String>) {
    let document = text_of(row, "document");
    let where_ = format!("part {:?}", document);
    let decoded = Part::from_wire_json(document);
    if text_of(row, "decode") != "ok" {
        let outcome = decoded.as_ref().map(|_| ()).map_err(Clone::clone);
        stage(
            outcome,
            false,
            text_of(row, "decode_error"),
            text_of(row, "decode_text_from"),
            &where_,
            failures,
        );
        return;
    }
    let part = match decoded {
        Ok(part) => part,
        Err(err) => {
            failures.push(format!(
                "{where_}: port refused with {err:?}, core decoded it"
            ));
            return;
        }
    };
    if part.kind().as_str() != text_of(row, "kind") {
        failures.push(format!(
            "{where_}: port calls it {:?}, core {:?}",
            part.kind().as_str(),
            text_of(row, "kind")
        ));
    }
    stage(
        part.validate(),
        text_of(row, "validate") == "ok",
        text_of(row, "validate_error"),
        "",
        &where_,
        failures,
    );
    written(part.to_wire_json(), row, &where_, failures);
}

fn replay_message(row: &Value, failures: &mut Vec<String>) {
    let document = text_of(row, "document");
    let where_ = format!("message {:?}", document);
    let decoded = Message::from_wire_json(document);
    if text_of(row, "decode") != "ok" {
        let outcome = decoded.as_ref().map(|_| ()).map_err(Clone::clone);
        stage(
            outcome,
            false,
            text_of(row, "decode_error"),
            text_of(row, "decode_text_from"),
            &where_,
            failures,
        );
        return;
    }
    let message = match decoded {
        Ok(message) => message,
        Err(err) => {
            failures.push(format!(
                "{where_}: port refused with {err:?}, core decoded it"
            ));
            return;
        }
    };
    if message.role.as_str() != text_of(row, "role") {
        failures.push(format!(
            "{where_}: port decodes role {:?}, core {:?}",
            message.role.as_str(),
            text_of(row, "role")
        ));
    }
    stage(
        message.validate(),
        text_of(row, "validate") == "ok",
        text_of(row, "validate_error"),
        "",
        &where_,
        failures,
    );
    written(message.to_wire_json(), row, &where_, failures);
}

fn replay_tool(row: &Value, failures: &mut Vec<String>) {
    let where_ = format!(
        "tool {}/{}/{}",
        text_of(row, "op"),
        text_of(row, "name"),
        text_of(row, "id")
    );
    let (validation, wire) = match text_of(row, "op") {
        "definition" => {
            let definition = ToolDefinition::new(
                text_of(row, "name"),
                text_of(row, "description"),
                text_of(row, "input_schema"),
            );
            (definition.validate(), definition.to_wire_json())
        }
        "call" => {
            let call = ToolCall {
                id: text_of(row, "id").to_owned(),
                name: text_of(row, "name").to_owned(),
                arguments: text_of(row, "arguments").to_owned(),
            };
            (call.validate(), call.to_wire_json())
        }
        "result" => {
            let result = ToolResult {
                call_id: text_of(row, "call_id").to_owned(),
                content: Content::text(text_of(row, "text")),
                is_error: row["is_error"].as_bool().unwrap_or(false),
            };
            (result.validate(), result.to_wire_json())
        }
        other => {
            failures.push(format!("{where_}: the port does not know op {other:?}"));
            return;
        }
    };
    stage(
        validation,
        text_of(row, "validate") == "ok",
        text_of(row, "validate_error"),
        "",
        &where_,
        failures,
    );
    written(wire, row, &where_, failures);
}

fn replay_schema(row: &Value, failures: &mut Vec<String>) {
    let where_ = format!("schema {:?}", text_of(row, "name"));
    let properties = samples(row, "props").iter().map(build_property).collect();
    let mut builder = define_schema(
        text_of(row, "name"),
        text_of(row, "description"),
        properties,
    );
    if row["closed"].as_bool().unwrap_or(false) {
        builder = builder.disallow_additional_properties();
    }
    let required: Vec<&str> = samples(row, "required")
        .iter()
        .filter_map(Value::as_str)
        .collect();
    if !required.is_empty() {
        builder = builder.required(&required);
    }
    let definition = builder.build();
    if definition.input_schema != text_of(row, "schema") {
        failures.push(format!(
            "{where_}: port built {:?}, core {:?}",
            definition.input_schema,
            text_of(row, "schema")
        ));
    }
    stage(
        definition.validate(),
        text_of(row, "validate") == "ok",
        text_of(row, "validate_error"),
        "",
        &where_,
        failures,
    );
    match definition.to_wire_json() {
        Ok(json) if json == text_of(row, "wire") => {}
        Ok(json) => failures.push(format!(
            "{where_}: port wrote {json:?}, core {:?}",
            text_of(row, "wire")
        )),
        Err(err) => failures.push(format!("{where_}: port refused to write with {err:?}")),
    }
}

fn build_property(sample: &Value) -> ToolPropertyDef {
    let name = text_of(sample, "name");
    let description = text_of(sample, "description");
    let typ = text_of(sample, "type");
    match text_of(sample, "kind") {
        "array" => tool_array_property(name, description, parse_json(text_of(sample, "items"))),
        "object" => {
            let properties = parse_json(text_of(sample, "properties"));
            let empty = Map::new();
            let properties = properties.as_object().unwrap_or(&empty);
            tool_object_property(name, description, properties)
        }
        "string_map" => tool_string_map_property(name, description),
        "default" => tool_property_with_default(
            name,
            typ,
            description,
            parse_json(text_of(sample, "default")),
        ),
        "enum" => {
            let values: Vec<&str> = samples(sample, "values")
                .iter()
                .filter_map(Value::as_str)
                .collect();
            tool_enum_property(name, typ, description, &values)
        }
        _ => tool_property(name, typ, description),
    }
}

fn parse_json(raw: &str) -> Value {
    if raw.is_empty() {
        return Value::Null;
    }
    serde_json::from_str(raw).unwrap_or(Value::Null)
}

fn text_of<'a>(row: &'a Value, field: &str) -> &'a str {
    row.get(field).and_then(Value::as_str).unwrap_or_default()
}

fn samples<'a>(row: &'a Value, field: &str) -> &'a [Value] {
    row.get(field)
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or_default()
}

/// The three refusals the port makes a stage earlier than core, because its
/// types have no invalid state (see the module docs). The corpus cannot hold
/// them -- it records the stage a refusal came from -- so they are pinned here,
/// against the sentence core's later stage would answer with.
#[test]
fn refusals_core_defers_are_made_early() {
    let err = match Message::from_wire_json(
        r#"{"role":"bot","content":{"parts":[{"type":"text","text":"hi"}]}}"#,
    ) {
        Ok(_) => panic!("core's sentence for this document comes from Validate"),
        Err(err) => err,
    };
    assert_eq!(err, "unknown message role \"bot\"");

    let err = match Part::from_wire_json(r#"{"type":"image"}"#) {
        Ok(_) => panic!("core's sentence for this document comes from Validate"),
        Err(err) => err,
    };
    assert_eq!(err, "unknown media source kind \"\"");

    let err = match Part::from_wire_json(
        r#"{"type":"audio","source":{"kind":"url","url":"https://x/a"},"format":{"encoding":"vorbis"}}"#,
    ) {
        Ok(_) => panic!("core's sentence for this document comes from Validate"),
        Err(err) => err,
    };
    assert_eq!(err, "content part: unknown audio encoding \"vorbis\"");
}

//! Replays the committed media contract against the port.
//!
//! `rust/conformance/message-contract.json` is dumped from core by
//! `tools/conformance-oracle -message-contract` and checked in; the gate fails
//! if regenerating it produces anything else. This test is the other half: it
//! rebuilds every sample -- a constructor call, a wire document, a format --
//! and holds the port's answer to core's, sentence by sentence.
//!
//! Three rows in the media table are refusals *by encoding/json* rather than
//! by core: the unknown field, the bad base64 payload, and a second document
//! after the first. The corpus marks them (`text_from`), because the port
//! refuses them in its own words -- a hand-written decoder does not reproduce
//! another library's syntax diagnostics -- and this test holds them to being
//! refused at all. The Go sentences are recorded here so a reader can see what
//! is not being compared:
//!
//! ```text
//! json: unknown field "extra"
//! json: cannot unmarshal string into Go struct field .data of type []uint8: illegal base64 data at input byte 0
//! invalid character '{' after top-level value
//! ```

use base64::Engine as _;
use flowcraft_contract::message::media::{
    AudioEncoding, AudioFormat, AudioSource, ImageSource, VideoSource,
};
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../conformance/message-contract.json");

/// One sample's answer: the normalized type, the base type, and the wire form.
type Answer = Result<(String, String, String), String>;

#[test]
fn the_media_contract_replays() {
    let document = contract();
    // A contract that lost its rows would still replay: everything in it
    // would pass. These floors are the same numbers the dump has today.
    let (media, formats) = (
        rows(&document, "media_sources").len(),
        rows(&document, "audio_formats").len(),
    );
    if media < 50 || formats < 15 {
        panic!("the contract has {media} media rows and {formats} formats; it used to have more");
    }
    let mut failures = Vec::new();
    for row in rows(&document, "media_sources") {
        replay_media(row, &mut failures);
    }
    for row in rows(&document, "audio_formats") {
        replay_format(row, &mut failures);
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

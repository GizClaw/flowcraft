//! The canonical content union: [`Part`], its nine kinds, and the wire codec
//! that writes and reads them.
//!
//! core's `message.Part` is a sealed Go interface with nine implementations,
//! plus a normalization step that collapses the pointer duplicates Go's method
//! set allows. Here it is an enum: one value per kind, no normalization, no
//! typed-nil hole. What survives from core is the vocabulary ([`PartKind`]) and
//! the JSON each variant is written as -- `{"type": ..., ...}`, the shape
//! `message.Content` uses for every entry of its `parts` array and that stream
//! deltas and protocol envelopes reuse.
//!
//! # What the decoder refuses, and in whose words
//!
//! Decoding is core's `UnmarshalPart`: the `type` header first, then a strict
//! decode of that kind's object, with `content part: ` in front of whatever the
//! inner decoder said. Two of those inner sentences are core's own and are
//! reproduced word for word (`content part has no text payload`, `content part
//! has unknown type %q`); the rest come from `encoding/json` and the port
//! spells them its own way -- the contract dump marks those rows, and the replay
//! holds them to being refusals rather than comparing text.
//!
//! # One divergence, on purpose
//!
//! A part whose `source` field is *absent* decodes in core (as the zero source)
//! and is refused later, by `Validate`, with `unknown media source kind ""`. The
//! port has no deconstructible source -- a [`ImageSource`] is valid by
//! construction -- so the same document is refused with the same sentence one
//! stage earlier, at decode. Same refusal, same words, different moment; the
//! corpus therefore samples absent sources only through the null spelling,
//! which core refuses at decode too.

use crate::json;
use crate::message::media::{AudioFormat, AudioSource, ImageSource, VideoSource};
use crate::message::tool::{ToolCall, ToolResult};
use crate::text;

/// Every canonical content part kind.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum PartKind {
    /// Visible text.
    Text,
    /// An image payload.
    Image,
    /// An audio payload.
    Audio,
    /// A video payload.
    Video,
    /// A reference to a file by URI.
    File,
    /// Structured data, always a JSON object.
    Data,
    /// A tool invocation the model asked for.
    ToolCall,
    /// The result of one tool invocation.
    ToolResult,
    /// A provider reasoning trace.
    Reasoning,
}

impl PartKind {
    /// Every kind, in core's declaration order. Tables that map kinds to
    /// behavior are pinned against this, so a new kind cannot be added without
    /// deciding what it means everywhere.
    pub const ALL: [PartKind; 9] = [
        PartKind::Text,
        PartKind::Image,
        PartKind::Audio,
        PartKind::Video,
        PartKind::File,
        PartKind::Data,
        PartKind::ToolCall,
        PartKind::ToolResult,
        PartKind::Reasoning,
    ];

    /// The kind's wire spelling.
    pub fn as_str(self) -> &'static str {
        match self {
            PartKind::Text => "text",
            PartKind::Image => "image",
            PartKind::Audio => "audio",
            PartKind::Video => "video",
            PartKind::File => "file",
            PartKind::Data => "data",
            PartKind::ToolCall => "tool_call",
            PartKind::ToolResult => "tool_result",
            PartKind::Reasoning => "reasoning",
        }
    }

    /// The kind a wire spelling names, when it names one.
    pub fn lookup(kind: &str) -> Option<PartKind> {
        match kind {
            "text" => Some(PartKind::Text),
            "image" => Some(PartKind::Image),
            "audio" => Some(PartKind::Audio),
            "video" => Some(PartKind::Video),
            "file" => Some(PartKind::File),
            "data" => Some(PartKind::Data),
            "tool_call" => Some(PartKind::ToolCall),
            "tool_result" => Some(PartKind::ToolResult),
            "reasoning" => Some(PartKind::Reasoning),
            _ => None,
        }
    }

    /// core's `PartKind.Validate` as a parse: the vocabulary is fixed, so an
    /// unknown name is the sentence core's validate would answer with. Decoding
    /// a part says something else for the same input -- see
    /// [`Part::from_wire_json`], which is core's `UnmarshalPart`.
    pub fn parse(kind: &str) -> Result<PartKind, String> {
        PartKind::lookup(kind)
            .ok_or_else(|| format!("unknown content part kind {}", text::quote(kind)))
    }
}

/// Visible text.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct TextPart {
    /// The text itself.
    pub text: String,
}

impl TextPart {
    /// A text part carrying `text`.
    pub fn new(text: impl Into<String>) -> Self {
        TextPart { text: text.into() }
    }
}

/// An image payload.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ImagePart {
    /// Where the image bytes live.
    pub source: ImageSource,
}

impl ImagePart {
    /// An image part over `source`.
    pub fn new(source: ImageSource) -> Self {
        ImagePart { source }
    }
}

/// An audio payload, with the format and duration a producer may know.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AudioPart {
    /// Where the audio bytes live.
    pub source: AudioSource,
    /// What the payload is, when the producer declares it.
    pub format: Option<AudioFormat>,
    /// How long the payload plays, when the producer knows it.
    pub duration_millis: Option<i64>,
}

impl AudioPart {
    /// An audio part over `source`, with nothing declared about it.
    pub fn new(source: AudioSource) -> Self {
        AudioPart {
            source,
            format: None,
            duration_millis: None,
        }
    }

    /// The same part with a declared format.
    pub fn with_format(mut self, format: AudioFormat) -> Self {
        self.format = Some(format);
        self
    }

    /// The same part with a declared duration.
    pub fn with_duration_millis(mut self, duration_millis: i64) -> Self {
        self.duration_millis = Some(duration_millis);
        self
    }
}

/// A video payload.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VideoPart {
    /// Where the video bytes live.
    pub source: VideoSource,
}

impl VideoPart {
    /// A video part over `source`.
    pub fn new(source: VideoSource) -> Self {
        VideoPart { source }
    }
}

/// A reference to a file, by URI rather than by payload.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct FilePart {
    /// Where the file is.
    pub uri: String,
    /// Its media type, when known.
    pub media_type: String,
    /// Its display name, when known.
    pub name: String,
}

impl FilePart {
    /// A file part for `uri`.
    pub fn new(uri: impl Into<String>) -> Self {
        FilePart {
            uri: uri.into(),
            media_type: String::new(),
            name: String::new(),
        }
    }
}

/// Structured data: a JSON object, kept as text the way core keeps it as
/// `json.RawMessage`.
///
/// The text is what the wire carries; it is compacted (not re-rendered) on the
/// way out, so a caller's key order and number spelling survive.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct DataPart {
    /// The media type of the payload, when known.
    pub media_type: String,
    /// A JSON object, as text.
    pub value: String,
}

impl DataPart {
    /// A data part carrying `value`, which [`DataPart::validate`] holds to
    /// being a JSON object.
    pub fn new(value: impl Into<String>) -> Self {
        DataPart {
            media_type: String::new(),
            value: value.into(),
        }
    }
}

/// A tool invocation the model asked for.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ToolCallPart {
    /// The call.
    pub call: ToolCall,
}

/// The result of one tool invocation.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ToolResultPart {
    /// The result.
    pub result: ToolResult,
}

/// A provider reasoning trace: the thinking a model produced while composing
/// the answer.
///
/// It is a trace, not a requested artifact: reasoning-capable models emit it
/// whether or not the request asked for reasoning, so responses may always
/// carry it. `text` empty with `signature` set is how a provider says
/// "reasoning happened, content withheld"; the convention is derived from the
/// fields rather than carried as a flag, as in core.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ReasoningPart {
    /// The visible reasoning, empty when the provider withheld it.
    pub text: String,
    /// The provider-issued opaque verification payload, replayed verbatim.
    pub signature: String,
    /// The provider-issued trace identifier, when the provider has one.
    pub id: String,
    /// Opaque provenance: the scope the producing driver stamped on the trace.
    pub source: String,
}

/// One canonical content part.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Part {
    /// See [`TextPart`].
    Text(TextPart),
    /// See [`ImagePart`].
    Image(ImagePart),
    /// See [`AudioPart`].
    Audio(AudioPart),
    /// See [`VideoPart`].
    Video(VideoPart),
    /// See [`FilePart`].
    File(FilePart),
    /// See [`DataPart`].
    Data(DataPart),
    /// See [`ToolCallPart`].
    ToolCall(ToolCallPart),
    /// See [`ToolResultPart`].
    ToolResult(ToolResultPart),
    /// See [`ReasoningPart`].
    Reasoning(ReasoningPart),
}

impl Part {
    /// The part's kind.
    pub fn kind(&self) -> PartKind {
        match self {
            Part::Text(_) => PartKind::Text,
            Part::Image(_) => PartKind::Image,
            Part::Audio(_) => PartKind::Audio,
            Part::Video(_) => PartKind::Video,
            Part::File(_) => PartKind::File,
            Part::Data(_) => PartKind::Data,
            Part::ToolCall(_) => PartKind::ToolCall,
            Part::ToolResult(_) => PartKind::ToolResult,
            Part::Reasoning(_) => PartKind::Reasoning,
        }
    }

    /// The kind's own payload rules -- core's `Part.Validate`, which every
    /// call site runs before the part is stored or sent.
    pub fn validate(&self) -> Result<(), String> {
        match self {
            Part::Text(_) => Ok(()),
            Part::Image(part) => part.source.validate(),
            Part::Audio(part) => validate_audio(part),
            Part::Video(part) => part.source.validate(),
            Part::File(part) => {
                if part.uri.is_empty() {
                    return Err("file URI is required".into());
                }
                Ok(())
            }
            Part::Data(part) => validate_data(part),
            Part::ToolCall(part) => part.call.validate(),
            Part::ToolResult(part) => part.result.validate(),
            Part::Reasoning(part) => {
                if part.text.is_empty() && part.signature.is_empty() {
                    return Err("reasoning part carries neither text nor signature".into());
                }
                Ok(())
            }
        }
    }

    /// The wire form: a type-discriminated object, `{"type": ..., ...}`.
    ///
    /// Like core's `MarshalPart`, this does not validate the part first -- the
    /// media sources validate themselves (a source that cannot be written is an
    /// error here), and everything else is written as it stands. `Content`
    /// validates before it marshals, and that is the path callers use.
    pub fn to_wire_json(&self) -> Result<String, String> {
        match self {
            Part::Text(part) => {
                let mut out = String::from("{\"type\":\"text\",\"text\":");
                json::write_string(&part.text, &mut out);
                out.push('}');
                Ok(out)
            }
            Part::Image(part) => Ok(format!(
                "{{\"type\":\"image\",\"source\":{}}}",
                part.source.to_wire_json()?
            )),
            Part::Audio(part) => {
                let mut out = format!(
                    "{{\"type\":\"audio\",\"source\":{}",
                    part.source.to_wire_json()?
                );
                if let Some(format) = &part.format {
                    out.push_str(",\"format\":");
                    out.push_str(&format.to_wire_json());
                }
                if let Some(duration) = part.duration_millis {
                    out.push_str(&format!(",\"duration_millis\":{duration}"));
                }
                out.push('}');
                Ok(out)
            }
            Part::Video(part) => Ok(format!(
                "{{\"type\":\"video\",\"source\":{}}}",
                part.source.to_wire_json()?
            )),
            Part::File(part) => {
                let mut out = String::from("{\"type\":\"file\",\"uri\":");
                json::write_string(&part.uri, &mut out);
                if !part.media_type.is_empty() {
                    out.push_str(",\"media_type\":");
                    json::write_string(&part.media_type, &mut out);
                }
                if !part.name.is_empty() {
                    out.push_str(",\"name\":");
                    json::write_string(&part.name, &mut out);
                }
                out.push('}');
                Ok(out)
            }
            Part::Data(part) => {
                let mut out = String::from("{\"type\":\"data\"");
                if !part.media_type.is_empty() {
                    out.push_str(",\"media_type\":");
                    json::write_string(&part.media_type, &mut out);
                }
                out.push_str(",\"value\":");
                if part.value.is_empty() {
                    // A nil `json.RawMessage` marshals as `null`, which is what
                    // an absent value is here; `Validate` refuses it.
                    out.push_str("null");
                } else if json::is_valid(&part.value) {
                    out.push_str(&json::compact(&part.value));
                } else {
                    return Err("json: data part value is not valid JSON".into());
                }
                out.push('}');
                Ok(out)
            }
            Part::ToolCall(part) => Ok(format!(
                "{{\"type\":\"tool_call\",\"call\":{}}}",
                part.call.to_wire_json()?
            )),
            Part::ToolResult(part) => Ok(format!(
                "{{\"type\":\"tool_result\",\"result\":{}}}",
                part.result.to_wire_json()?
            )),
            Part::Reasoning(part) => {
                let mut out = String::from("{\"type\":\"reasoning\"");
                for (name, value) in [
                    ("text", part.text.as_str()),
                    ("signature", part.signature.as_str()),
                    ("id", part.id.as_str()),
                    ("source", part.source.as_str()),
                ] {
                    if !value.is_empty() {
                        out.push_str(&format!(",\"{name}\":"));
                        json::write_string(value, &mut out);
                    }
                }
                out.push('}');
                Ok(out)
            }
        }
    }

    /// core's `UnmarshalPart`: the `type` header, then that kind's strict
    /// object. The part is *not* validated -- see the module docs for what
    /// core defers and why the port sometimes cannot.
    pub fn from_wire_json(document: &str) -> Result<Part, String> {
        let header =
            json::Members::parse(document).map_err(|err| format!("content part: {err}"))?;
        let name = header
            .string("type")
            .map_err(|err| format!("content part: {err}"))?;
        let kind = PartKind::lookup(&name)
            .ok_or_else(|| format!("content part has unknown type {}", text::quote(&name)))?;
        match kind {
            PartKind::Text => {
                let members = strict(document, &["type", "text"])?;
                if members.raw("text").is_none_or(|raw| raw == "null") {
                    return Err("content part has no text payload".into());
                }
                Ok(Part::Text(TextPart {
                    text: members
                        .string("text")
                        .map_err(|err| format!("content part: {err}"))?,
                }))
            }
            PartKind::Image => {
                let members = strict(document, &["type", "source"])?;
                Ok(Part::Image(ImagePart {
                    source: source(members.raw("source"), ImageSource::from_wire_json)?,
                }))
            }
            PartKind::Audio => {
                let members = strict(document, &["type", "source", "format", "duration_millis"])?;
                let format = match members.raw("format") {
                    None | Some("null") => None,
                    Some(raw) => Some(
                        AudioFormat::from_wire_json(raw)
                            .map_err(|err| format!("content part: {err}"))?,
                    ),
                };
                Ok(Part::Audio(AudioPart {
                    source: source(members.raw("source"), AudioSource::from_wire_json)?,
                    format,
                    duration_millis: members
                        .i64("duration_millis")
                        .map_err(|err| format!("content part: {err}"))?,
                }))
            }
            PartKind::Video => {
                let members = strict(document, &["type", "source"])?;
                Ok(Part::Video(VideoPart {
                    source: source(members.raw("source"), VideoSource::from_wire_json)?,
                }))
            }
            PartKind::File => {
                let members = strict(document, &["type", "uri", "media_type", "name"])?;
                Ok(Part::File(FilePart {
                    uri: members
                        .string("uri")
                        .map_err(|err| format!("content part: {err}"))?,
                    media_type: members
                        .string("media_type")
                        .map_err(|err| format!("content part: {err}"))?,
                    name: members
                        .string("name")
                        .map_err(|err| format!("content part: {err}"))?,
                }))
            }
            PartKind::Data => {
                let members = strict(document, &["type", "media_type", "value"])?;
                Ok(Part::Data(DataPart {
                    media_type: members
                        .string("media_type")
                        .map_err(|err| format!("content part: {err}"))?,
                    value: members.raw("value").unwrap_or_default().to_owned(),
                }))
            }
            PartKind::ToolCall => {
                let members = strict(document, &["type", "call"])?;
                let call = match members.raw("call") {
                    None | Some("null") => ToolCall::default(),
                    Some(raw) => ToolCall::from_wire_json(raw)
                        .map_err(|err| format!("content part: {err}"))?,
                };
                Ok(Part::ToolCall(ToolCallPart { call }))
            }
            PartKind::ToolResult => {
                let members = strict(document, &["type", "result"])?;
                let result = match members.raw("result") {
                    None | Some("null") => ToolResult::default(),
                    Some(raw) => ToolResult::from_wire_json(raw)
                        .map_err(|err| format!("content part: {err}"))?,
                };
                Ok(Part::ToolResult(ToolResultPart { result }))
            }
            PartKind::Reasoning => {
                let members = strict(document, &["type", "text", "signature", "id", "source"])?;
                let field = |name: &str| -> Result<String, String> {
                    members
                        .string(name)
                        .map_err(|err| format!("content part: {err}"))
                };
                Ok(Part::Reasoning(ReasoningPart {
                    text: field("text")?,
                    signature: field("signature")?,
                    id: field("id")?,
                    source: field("source")?,
                }))
            }
        }
    }
}

/// The strict pass core runs after the header: unknown members refused in
/// document order, with `encoding/json`'s sentence for them.
fn strict<'a>(document: &'a str, allowed: &[&str]) -> Result<json::Members<'a>, String> {
    let members = json::Members::parse(document).map_err(|err| format!("content part: {err}"))?;
    if let Some(unknown) = members.first_unknown(allowed) {
        return Err(format!(
            "content part: json: unknown field {}",
            text::quote(unknown)
        ));
    }
    Ok(members)
}

/// The source a part carries, refusing an absent one with the sentence core's
/// `Validate` would answer with (see the module docs).
fn source<T>(raw: Option<&str>, decode: fn(&str) -> Result<T, String>) -> Result<T, String> {
    match raw {
        Some(raw) => decode(raw).map_err(|err| format!("content part: {err}")),
        None => Err("unknown media source kind \"\"".into()),
    }
}

fn validate_audio(part: &AudioPart) -> Result<(), String> {
    part.source.validate()?;
    if let Some(format) = &part.format {
        format.validate()?;
        if !part.source.media_type().is_empty()
            && part.source.base_media_type() != format.encoding.media_type()
        {
            return Err("audio source media type does not match its format".into());
        }
    }
    if part.duration_millis.is_some_and(|duration| duration < 0) {
        return Err("audio duration must not be negative".into());
    }
    Ok(())
}

fn validate_data(part: &DataPart) -> Result<(), String> {
    let value = part.value.trim();
    if value.is_empty() || !value.starts_with('{') || !json::is_valid(value) {
        return Err("data part value must be a JSON object".into());
    }
    Ok(())
}

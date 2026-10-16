//! The media DTOs that ride inside multimodal message parts.
//!
//! core keeps these in `core/message/media`: operation-neutral value types for
//! image, audio and video payloads, so a part can carry bytes, an absolute URL
//! or a live stream without the package above choosing an operation. The port
//! takes the serializable half:
//!
//! - sources -- [`ImageSource`], [`AudioSource`], [`VideoSource`] -- whose only
//!   difference is the media-type family each one may carry, which here is a
//!   type, not a runtime check;
//! - formats, geometry and quality: [`AudioFormat`], [`ImageFormat`],
//!   [`ImageSize`], [`AspectRatio`], [`ImageQuality`], [`VideoFrame`].
//!
//! And leaves the live half to the media slice, on purpose:
//!
//! - `Stream`/`Pipe` (the bounded pull transport) and `AudioChunk` are not
//!   shapes: nothing about them survives a round trip, and core refuses to
//!   serialize a stream source at all. [`SourceKind`] therefore has no stream
//!   variant, and the wire decode refuses one with core's sentence;
//! - `AudioDurationMillis` is a decoder -- raw PCM arithmetic and an MPEG frame
//!   walk -- and lands where durations are used;
//! - `VoiceSpec` is a request-side knob for the text-to-speech slice.
//!
//! # Media types
//!
//! Every source stores a normalized media type, and "normalized" means
//! `mime.ParseMediaType` followed by `mime.FormatMediaType`, which is where
//! this module does the most work per line of code: the type is lowercased
//! (`IMAGE/PNG` is `image/png`), parameter names are lowercased, quoted values
//! are unquoted, duplicates are refused, and formatting puts the parameters
//! back in key order with a space after each semicolon
//! (`image/png; name=result.png`). The type must stay inside the family the
//! source declares -- an image source is `image/*` -- and that check is what
//! `media type "audio/mpeg" must use image/` is about.
//!
//! One part of Go's parser is deliberately not here: RFC 2231/2047 parameter
//! encodings (`name*=utf-8''...`, `name*0*=...`). Nothing in this repository
//! produces them, and a caller that hands one in gets a refusal where core
//! would decode it -- a divergence to keep in view, listed here and not in the
//! gate's exemption list until a document actually carries one.

use crate::json;
use crate::text;

/// How a serializable source carries its payload.
///
/// core also has a `stream` kind, which is a live handle rather than a
/// payload; see the module docs.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum SourceKind {
    /// An absolute URL the consumer fetches itself.
    Url,
    /// Inline bytes, base64 in the wire form.
    Inline,
}

impl SourceKind {
    /// The kind's wire spelling.
    pub fn as_str(self) -> &'static str {
        match self {
            SourceKind::Url => "url",
            SourceKind::Inline => "inline",
        }
    }
}

/// The payload rules the three typed sources share.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Source {
    kind: SourceKind,
    url: String,
    data: Vec<u8>,
    media_type: String,
}

impl Source {
    /// core's `newURL`: the URL is checked first, then the media type, which
    /// urls may omit.
    fn new_url(raw_url: &str, media_type: &str, prefix: &str) -> Result<Source, String> {
        if !is_absolute_request_url(raw_url) {
            return Err("media URL must be absolute".into());
        }
        let media_type = normalize_media_type(media_type, prefix, false)?;
        let source = Source {
            kind: SourceKind::Url,
            url: raw_url.to_owned(),
            data: Vec::new(),
            media_type,
        };
        source.validate()?;
        Ok(source)
    }

    /// core's `newBytes`: the media type is checked first (and required), then
    /// the bytes are copied; the source owns what it carries.
    fn new_inline(data: &[u8], media_type: &str, prefix: &str) -> Result<Source, String> {
        let media_type = normalize_media_type(media_type, prefix, true)?;
        let source = Source {
            kind: SourceKind::Inline,
            url: String::new(),
            data: data.to_vec(),
            media_type,
        };
        source.validate()?;
        Ok(source)
    }

    /// The kind's own payload rules, without the media-type family check.
    fn validate(&self) -> Result<(), String> {
        match self.kind {
            SourceKind::Url => {
                if self.url.is_empty() || !self.data.is_empty() {
                    return Err("URL media source has invalid payload".into());
                }
            }
            SourceKind::Inline => {
                if self.data.is_empty() || !self.url.is_empty() {
                    return Err("inline media source has invalid payload".into());
                }
                if self.media_type.is_empty() {
                    return Err("inline media source media type is required".into());
                }
            }
        }
        Ok(())
    }

    /// core's `validateTypedSource`: the payload rules, then the family check.
    fn validate_typed(&self, prefix: &str) -> Result<(), String> {
        self.validate()?;
        normalize_media_type(&self.media_type, prefix, self.kind == SourceKind::Inline)?;
        Ok(())
    }

    /// The same, keeping the normalized media type -- what the wire decode
    /// stores.
    fn normalized(mut self, prefix: &str) -> Result<Source, String> {
        self.validate()?;
        self.media_type =
            normalize_media_type(&self.media_type, prefix, self.kind == SourceKind::Inline)?;
        Ok(self)
    }

    /// core's `MarshalJSON`: the payload rules again (a stream source never
    /// gets this far here), then the wire object with the fields the source
    /// has -- kind, url, data, media_type, in that order, empty ones omitted.
    fn to_wire_json(&self) -> Result<String, String> {
        self.validate()?;
        let mut out = String::from("{\"kind\":");
        json::write_string(self.kind.as_str(), &mut out);
        if !self.url.is_empty() {
            out.push_str(",\"url\":");
            json::write_string(&self.url, &mut out);
        }
        if !self.data.is_empty() {
            out.push_str(",\"data\":");
            json::write_string(&base64_encode(&self.data), &mut out);
        }
        if !self.media_type.is_empty() {
            out.push_str(",\"media_type\":");
            json::write_string(&self.media_type, &mut out);
        }
        out.push('}');
        Ok(out)
    }

    /// core's `source.UnmarshalJSON`, with the typed checks a caller adds on
    /// top of it ([`Source::normalized`]).
    fn from_wire_json(document: &str) -> Result<Source, String> {
        let value: serde_json::Value =
            serde_json::from_str(document).map_err(|err| format!("media source: {err}"))?;
        let serde_json::Value::Object(object) = value else {
            return Err("media source: not a JSON object".into());
        };
        for key in object.keys() {
            if !matches!(key.as_str(), "kind" | "url" | "data" | "media_type") {
                return Err(format!("json: unknown field {}", text::quote(key)));
            }
        }
        let kind = match object.get("kind") {
            Some(serde_json::Value::String(kind)) => kind_from_wire(kind)?,
            _ => kind_from_wire("")?,
        };
        let url = optional_string(object.get("url"), "url")?;
        let media_type = optional_string(object.get("media_type"), "media_type")?;
        let data = match object.get("data") {
            Some(serde_json::Value::String(encoded)) => {
                base64_decode(encoded).map_err(|err| format!("media source data: {err}"))?
            }
            _ => Vec::new(),
        };
        let source = Source {
            kind,
            url,
            data,
            media_type,
        };
        source.validate()?;
        Ok(source)
    }
}

macro_rules! typed_source {
    ($name:ident, $prefix:literal, $doc:literal) => {
        #[doc = $doc]
        #[derive(Debug, Clone, PartialEq, Eq)]
        pub struct $name(Source);

        impl $name {
            /// An absolute-URL source. The media type may be empty; when it is
            /// not, it must be in this source's family.
            pub fn new_url(raw_url: &str, media_type: &str) -> Result<Self, String> {
                Source::new_url(raw_url, media_type, $prefix).map($name)
            }

            /// An inline-bytes source. The media type is required and must be
            /// in this source's family; the bytes are copied in.
            pub fn new_inline(data: &[u8], media_type: &str) -> Result<Self, String> {
                Source::new_inline(data, media_type, $prefix).map($name)
            }

            /// The wire object, byte for byte what core writes.
            pub fn to_wire_json(&self) -> Result<String, String> {
                self.0.to_wire_json()
            }

            /// Decodes the wire object, applying this source family's checks.
            pub fn from_wire_json(document: &str) -> Result<Self, String> {
                Source::from_wire_json(document)?
                    .normalized($prefix)
                    .map($name)
            }

            /// Whether the payload and the media type hold together.
            pub fn validate(&self) -> Result<(), String> {
                self.0.validate_typed($prefix)
            }

            /// How the payload travels.
            pub fn kind(&self) -> SourceKind {
                self.0.kind
            }

            /// The URL, empty when the source is inline.
            pub fn url(&self) -> &str {
                &self.0.url
            }

            /// A copy of the inline bytes, empty when the source is a URL.
            pub fn bytes(&self) -> Vec<u8> {
                self.0.data.clone()
            }

            /// The media type as stored, parameters included.
            pub fn media_type(&self) -> &str {
                &self.0.media_type
            }

            /// The media type without its parameters, or `""` when it does not
            /// parse.
            pub fn base_media_type(&self) -> &str {
                base_media_type(&self.0.media_type)
            }
        }
    };
}

typed_source!(
    ImageSource,
    "image/",
    "An image payload: inline bytes or an absolute URL, carrying an `image/*` media type."
);
typed_source!(
    AudioSource,
    "audio/",
    "An audio payload: inline bytes or an absolute URL, carrying an `audio/*` media type."
);
typed_source!(
    VideoSource,
    "video/",
    "A video payload: inline bytes or an absolute URL, carrying a `video/*` media type."
);

fn kind_from_wire(kind: &str) -> Result<SourceKind, String> {
    match kind {
        "url" => Ok(SourceKind::Url),
        "inline" => Ok(SourceKind::Inline),
        // core models live streams here; a wire document cannot carry one, so
        // decoding it is the payload rule failing, not an unknown kind.
        "stream" => Err("stream media source has invalid payload".into()),
        other => Err(format!("unknown media source kind {}", text::quote(other))),
    }
}

fn optional_string(field: Option<&serde_json::Value>, name: &str) -> Result<String, String> {
    match field {
        None | Some(serde_json::Value::Null) => Ok(String::new()),
        Some(serde_json::Value::String(value)) => Ok(value.clone()),
        Some(other) => Err(format!(
            "media source: {name} must be a string, not {other}"
        )),
    }
}

/// core checks `url.ParseRequestURI`, then that scheme and host are both
/// non-empty. What the port keeps of the parser is the shape a request URI has
/// -- a scheme, an authority, a host in it -- plus those two checks. A
/// fragment is *not* refused: the parser posts it, and the contract dump
/// samples `https://x/y#f` accepted.
///
/// A space or a control character in the *authority* is refused, which the
/// dump samples; a space in the path is not, which the dump samples too. The
/// rest of the host's character rules are not sampled and therefore not
/// claimed here.
fn is_absolute_request_url(url: &str) -> bool {
    let Some((scheme, rest)) = url.split_once(':') else {
        return false;
    };
    if !is_scheme(scheme) {
        return false;
    }
    let Some(rest) = rest.strip_prefix("//") else {
        return false;
    };
    // The authority runs to the path, the query, or the fragment.
    let host_end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let host = &rest[..host_end];
    !host.is_empty() && !host.contains(' ') && !host.chars().any(|c| c.is_ascii_control())
}

fn is_scheme(scheme: &str) -> bool {
    let mut chars = scheme.chars();
    match chars.next() {
        Some(first) if first.is_ascii_alphabetic() => {}
        _ => return false,
    }
    chars.all(|c| c.is_ascii_alphanumeric() || matches!(c, '+' | '-' | '.'))
}

/// core's `normalizeMediaType`: empty is allowed or refused depending on the
/// caller, a parse failure and a foreign family read the same way, and success
/// answers the formatted type.
fn normalize_media_type(value: &str, prefix: &str, required: bool) -> Result<String, String> {
    if value.is_empty() {
        if required {
            return Err("media source media type is required".into());
        }
        return Ok(String::new());
    }
    match parse_media_type(value) {
        Some((base, params)) if base.starts_with(prefix) => Ok(format_media_type(&base, &params)),
        _ => Err(format!(
            "media type {} must use {prefix}",
            text::quote(value)
        )),
    }
}

/// core's `BaseMediaType`: the type without parameters, `""` when it does not
/// parse. The stored value is normalized, so the split is the parse.
fn base_media_type(media_type: &str) -> &str {
    if parse_media_type(media_type).is_none() {
        return "";
    }
    match media_type.split_once(';') {
        Some((base, _)) => base,
        None => media_type,
    }
}

/// `mime.ParseMediaType`: the lowercased type and parameter names (values keep
/// their case), quoted values unquoted, duplicates refused. RFC 2231
/// continuations are not decoded -- see the module docs.
fn parse_media_type(value: &str) -> Option<(String, Vec<(String, String)>)> {
    let segments = split_media_type(value)?;
    let base = segments.first()?.trim().to_ascii_lowercase();
    let (major, minor) = base.split_once('/')?;
    if !is_token(major) || !is_token(minor) {
        return None;
    }
    let mut params: Vec<(String, String)> = Vec::new();
    for segment in &segments[1..] {
        let segment = segment.trim();
        if segment.is_empty() {
            continue;
        }
        let (name, raw_value) = segment.split_once('=')?;
        let name = name.trim().to_ascii_lowercase();
        if !is_token(&name) {
            return None;
        }
        let parsed = parse_param_value(raw_value.trim())?;
        if params.iter().any(|(existing, _)| *existing == name) {
            return None;
        }
        params.push((name, parsed));
    }
    Some((base, params))
}

/// Splits on the semicolons that are not inside a quoted value.
fn split_media_type(value: &str) -> Option<Vec<&str>> {
    let mut segments = Vec::new();
    let mut start = 0;
    let mut quoted = false;
    let mut escaped = false;
    for (index, ch) in value.char_indices() {
        if escaped {
            escaped = false;
            continue;
        }
        match ch {
            '\\' if quoted => escaped = true,
            '"' => quoted = !quoted,
            ';' if !quoted => {
                segments.push(&value[start..index]);
                start = index + 1;
            }
            _ => {}
        }
    }
    if quoted {
        return None;
    }
    segments.push(&value[start..]);
    Some(segments)
}

/// A parameter value: a token, or a quoted string with backslash escapes.
fn parse_param_value(raw: &str) -> Option<String> {
    let Some(rest) = raw.strip_prefix('"') else {
        return is_token(raw).then(|| raw.to_owned());
    };
    let mut out = String::new();
    let mut chars = rest.chars();
    while let Some(ch) = chars.next() {
        match ch {
            '"' => return chars.next().is_none().then_some(out),
            '\\' => out.push(chars.next()?),
            _ if ch.is_control() => return None,
            _ => out.push(ch),
        }
    }
    None
}

/// `mime.FormatMediaType`: the type, then the parameters sorted by name, a
/// value quoted when it is not a token.
fn format_media_type(base: &str, params: &[(String, String)]) -> String {
    let mut out = base.to_owned();
    let mut sorted: Vec<&(String, String)> = params.iter().collect();
    sorted.sort();
    for (name, value) in sorted {
        out.push_str("; ");
        out.push_str(name);
        out.push('=');
        if is_token(value) {
            out.push_str(value);
        } else {
            out.push('"');
            for ch in value.chars() {
                if ch == '"' || ch == '\\' {
                    out.push('\\');
                }
                out.push(ch);
            }
            out.push('"');
        }
    }
    out
}

/// RFC 7230's token characters, which is what `mime` accepts unquoted.
fn is_token(value: &str) -> bool {
    !value.is_empty()
        && value
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "!#$%&'*+-.^_`|~".contains(c))
}

fn base64_encode(data: &[u8]) -> String {
    use base64::Engine as _;
    base64::engine::general_purpose::STANDARD.encode(data)
}

fn base64_decode(encoded: &str) -> Result<Vec<u8>, String> {
    use base64::Engine as _;
    base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .map_err(|err| err.to_string())
}

/// How an audio payload is encoded.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum AudioEncoding {
    /// Signed 16-bit PCM.
    Pcm16,
    /// Signed 24-bit PCM.
    Pcm24,
    /// 32-bit float PCM.
    Float32,
    /// MPEG audio.
    Mp3,
    /// Opus.
    Opus,
    /// AAC.
    Aac,
    /// FLAC.
    Flac,
}

impl AudioEncoding {
    /// The encoding's wire spelling.
    pub fn as_str(self) -> &'static str {
        match self {
            AudioEncoding::Pcm16 => "pcm16",
            AudioEncoding::Pcm24 => "pcm24",
            AudioEncoding::Float32 => "float32",
            AudioEncoding::Mp3 => "mp3",
            AudioEncoding::Opus => "opus",
            AudioEncoding::Aac => "aac",
            AudioEncoding::Flac => "flac",
        }
    }

    /// The encoding a wire spelling names, or `None`.
    pub fn parse(name: &str) -> Option<Self> {
        match name {
            "pcm16" => Some(AudioEncoding::Pcm16),
            "pcm24" => Some(AudioEncoding::Pcm24),
            "float32" => Some(AudioEncoding::Float32),
            "mp3" => Some(AudioEncoding::Mp3),
            "opus" => Some(AudioEncoding::Opus),
            "aac" => Some(AudioEncoding::Aac),
            "flac" => Some(AudioEncoding::Flac),
            _ => None,
        }
    }

    /// The media type the encoding family belongs to, or `""` for none.
    pub fn media_type(self) -> &'static str {
        match self {
            AudioEncoding::Pcm16 | AudioEncoding::Pcm24 | AudioEncoding::Float32 => "audio/pcm",
            AudioEncoding::Mp3 => "audio/mpeg",
            AudioEncoding::Opus => "audio/opus",
            AudioEncoding::Aac => "audio/aac",
            AudioEncoding::Flac => "audio/flac",
        }
    }

    /// Whether the encoding is raw PCM, which is the family whose format
    /// requirements are strict.
    pub fn is_raw(self) -> bool {
        matches!(
            self,
            AudioEncoding::Pcm16 | AudioEncoding::Pcm24 | AudioEncoding::Float32
        )
    }
}

/// The parameters a payload needs to be interpretable. A raw PCM encoding
/// needs a sample rate and a channel count; the compressed ones may leave both
/// out (and both are then absent).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AudioFormat {
    /// The encoding family.
    pub encoding: AudioEncoding,
    /// Samples per second, when the payload declares them. core's zero means
    /// the same thing as the port's absent one, and a negative is refused
    /// below rather than read as "not declared".
    pub sample_rate_hz: Option<i32>,
    /// Channel count, when the payload declares it.
    pub channels: Option<i32>,
}

impl AudioFormat {
    /// A format with everything the caller knows.
    pub fn new(
        encoding: AudioEncoding,
        sample_rate_hz: Option<i32>,
        channels: Option<i32>,
    ) -> Self {
        AudioFormat {
            encoding,
            sample_rate_hz,
            channels,
        }
    }

    /// core's `AudioFormat.Validate`: the raw families need both numbers and
    /// need them positive; the compressed ones read a zero as "not declared"
    /// and refuse only negatives.
    ///
    /// core's third branch -- `unknown audio encoding %q` -- has no reachable
    /// input here: an encoding outside the table cannot be built, so that
    /// refusal belongs to the decode that reads one (and is written there).
    pub fn validate(&self) -> Result<(), String> {
        let rate = self.sample_rate_hz.unwrap_or(0);
        let channels = self.channels.unwrap_or(0);
        if self.encoding.is_raw() {
            if rate <= 0 || channels <= 0 {
                return Err("raw audio format requires sample rate and channels".into());
            }
            return Ok(());
        }
        if rate < 0 || channels < 0 {
            return Err("audio sample rate and channels must not be negative".into());
        }
        Ok(())
    }
}

/// The image codecs a source may declare.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum ImageFormat {
    /// PNG.
    Png,
    /// JPEG.
    Jpeg,
    /// WebP.
    WebP,
    /// GIF.
    Gif,
    /// AVIF.
    Avif,
}

impl ImageFormat {
    /// The format's wire spelling.
    pub fn as_str(self) -> &'static str {
        match self {
            ImageFormat::Png => "png",
            ImageFormat::Jpeg => "jpeg",
            ImageFormat::WebP => "webp",
            ImageFormat::Gif => "gif",
            ImageFormat::Avif => "avif",
        }
    }

    /// The format a wire spelling names, or `None`.
    pub fn parse(name: &str) -> Option<Self> {
        match name {
            "png" => Some(ImageFormat::Png),
            "jpeg" => Some(ImageFormat::Jpeg),
            "webp" => Some(ImageFormat::WebP),
            "gif" => Some(ImageFormat::Gif),
            "avif" => Some(ImageFormat::Avif),
            _ => None,
        }
    }

    /// The media type of the format.
    pub fn media_type(self) -> &'static str {
        match self {
            ImageFormat::Png => "image/png",
            ImageFormat::Jpeg => "image/jpeg",
            ImageFormat::WebP => "image/webp",
            ImageFormat::Gif => "image/gif",
            ImageFormat::Avif => "image/avif",
        }
    }
}

/// The canonical generation quality tiers. The values mirror the `gpt-image`
/// family's quality parameter, the only image provider in the repository with
/// a per-request quality knob.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum ImageQuality {
    /// The provider's own default.
    Auto,
    /// Speed over quality.
    Low,
    /// The middle tier.
    Medium,
    /// The best tier.
    High,
    /// The tier above high, published by the newest GPT image models.
    XHigh,
    /// The top tier, published by the same models as [`ImageQuality::XHigh`].
    Max,
}

impl ImageQuality {
    /// The quality's wire spelling.
    pub fn as_str(self) -> &'static str {
        match self {
            ImageQuality::Auto => "auto",
            ImageQuality::Low => "low",
            ImageQuality::Medium => "medium",
            ImageQuality::High => "high",
            ImageQuality::XHigh => "xhigh",
            ImageQuality::Max => "max",
        }
    }

    /// The quality a wire spelling names, or `None`.
    pub fn parse(name: &str) -> Option<Self> {
        match name {
            "auto" => Some(ImageQuality::Auto),
            "low" => Some(ImageQuality::Low),
            "medium" => Some(ImageQuality::Medium),
            "high" => Some(ImageQuality::High),
            "xhigh" => Some(ImageQuality::XHigh),
            "max" => Some(ImageQuality::Max),
            _ => None,
        }
    }
}

/// An image's pixel dimensions.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ImageSize {
    /// Width in pixels.
    pub width: u32,
    /// Height in pixels.
    pub height: u32,
}

impl ImageSize {
    /// A size with both dimensions.
    pub fn new(width: u32, height: u32) -> Self {
        ImageSize { width, height }
    }

    /// core's check: both dimensions have to be positive.
    pub fn validate(&self) -> Result<(), String> {
        if self.width == 0 || self.height == 0 {
            return Err("image dimensions must be positive".into());
        }
        Ok(())
    }
}

/// A `width:height` aspect ratio, e.g. `16:9`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AspectRatio(String);

impl AspectRatio {
    /// An aspect ratio from its spelling.
    pub fn new(value: impl Into<String>) -> Self {
        AspectRatio(value.into())
    }

    /// The spelling.
    pub fn as_str(&self) -> &str {
        &self.0
    }

    /// core's check: a `:` with two positive integers around it.
    pub fn validate(&self) -> Result<(), String> {
        let Some((width, height)) = self.0.split_once(':') else {
            return Err("aspect ratio must use width:height".into());
        };
        let (Ok(width), Ok(height)) = (width.parse::<i64>(), height.parse::<i64>()) else {
            return Err("aspect ratio components must be positive integers".into());
        };
        if width <= 0 || height <= 0 {
            return Err("aspect ratio components must be positive integers".into());
        }
        Ok(())
    }
}

/// A timestamped image frame for live multimodal input. Complete video
/// generation request and response types are out of scope, as in core.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VideoFrame {
    /// The frame's pixels.
    pub source: ImageSource,
    /// Where the frame sits in the stream, in milliseconds. Absent is zero.
    pub timestamp_millis: i64,
}

impl VideoFrame {
    /// core's `VideoFrame.Validate`: the source first, then the timestamp.
    pub fn validate(&self) -> Result<(), String> {
        self.source.validate()?;
        if self.timestamp_millis < 0 {
            return Err("video frame timestamp must not be negative".into());
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Unwraps a rule the test expects to hold; a refusal fails the test with
    /// its own sentence instead of a backtrace line.
    fn ok<T>(result: Result<T, String>, what: &str) -> T {
        match result {
            Ok(value) => value,
            Err(err) => panic!("{what}: {err}"),
        }
    }

    #[test]
    fn inline_sources_own_their_bytes() {
        let mut raw = b"audio".to_vec();
        let source = ok(
            AudioSource::new_inline(&raw, "audio/pcm"),
            "valid inline source",
        );
        raw[0] = b'X';
        assert_eq!(source.bytes(), b"audio");
        let mut copy_out = source.bytes();
        copy_out[0] = b'X';
        assert_eq!(source.bytes(), b"audio");
    }

    #[test]
    fn url_sources_carry_an_absolute_url() {
        let image = ok(
            ImageSource::new_url("https://example.com/image.png", "image/png"),
            "absolute url",
        );
        assert_eq!(image.kind(), SourceKind::Url);
        assert_eq!(image.url(), "https://example.com/image.png");
        assert_eq!(image.bytes(), Vec::<u8>::new());

        // Relative and schemeless urls are not request URIs. A fragment is
        // fine: core's parser posts it rather than refusing it (the contract
        // dump samples this).
        let fragment = ok(
            ImageSource::new_url("https://example.com/i#f", "image/png"),
            "fragment url",
        );
        assert_eq!(fragment.url(), "https://example.com/i#f");
        for url in ["/image.png", "example.com/image.png"] {
            let refused = ImageSource::new_url(url, "image/png");
            assert_eq!(
                refused,
                Err("media URL must be absolute".to_owned()),
                "{url}"
            );
        }
    }

    #[test]
    fn media_types_are_normalized_the_way_core_normalizes_them() {
        let image = ok(
            ImageSource::new_url("https://example.com/image", "IMAGE/PNG; name=result.png"),
            "normalized image url",
        );
        assert_eq!(image.media_type(), "image/png; name=result.png");
        assert_eq!(image.base_media_type(), "image/png");

        let audio = ok(
            AudioSource::new_url("https://example.com/audio", "audio/webm; codecs=opus"),
            "parameterized audio url",
        );
        assert_eq!(audio.media_type(), "audio/webm; codecs=opus");
        assert_eq!(audio.base_media_type(), "audio/webm");

        // An url source may leave the type out; an inline one may not.
        let bare = ok(
            ImageSource::new_url("https://example.com/image", ""),
            "no media type",
        );
        assert_eq!(bare.media_type(), "");
        assert_eq!(bare.base_media_type(), "");
        assert_eq!(
            AudioSource::new_inline(b"pcm", ""),
            Err("media source media type is required".to_owned())
        );
    }

    #[test]
    fn typed_sources_refuse_another_family_s_media_type() {
        assert_eq!(
            ImageSource::new_inline(b"audio", "audio/mpeg"),
            Err("media type \"audio/mpeg\" must use image/".to_owned())
        );
        assert_eq!(
            AudioSource::new_url("https://example.com/audio", "video/mp4"),
            Err("media type \"video/mp4\" must use audio/".to_owned())
        );
        assert_eq!(
            VideoSource::new_inline(b"video", "image/jpeg"),
            Err("media type \"image/jpeg\" must use video/".to_owned())
        );
        // Not even a prefix that looks right: the family is `image/`.
        assert_eq!(
            ImageSource::new_inline(b"x", "image"),
            Err("media type \"image\" must use image/".to_owned())
        );
    }

    #[test]
    fn payload_rules_read_the_way_core_reads_them() {
        assert_eq!(
            ImageSource::new_inline(b"", "image/png"),
            Err("inline media source has invalid payload".to_owned())
        );
        assert_eq!(
            ImageSource::from_wire_json(r#"{"kind":"url"}"#),
            Err("URL media source has invalid payload".to_owned())
        );
    }

    #[test]
    fn the_wire_form_round_trips_and_reads_like_core_s() {
        let image = ok(
            ImageSource::new_inline(b"secret image", "image/png"),
            "inline",
        );
        let wire = ok(image.to_wire_json(), "serializable");
        assert_eq!(
            wire,
            r#"{"kind":"inline","data":"c2VjcmV0IGltYWdl","media_type":"image/png"}"#
        );
        assert_eq!(ok(ImageSource::from_wire_json(&wire), "round trip"), image);

        let url = ok(
            VideoSource::new_url("https://example.com/v.mp4", "video/mp4"),
            "url",
        );
        let wire = ok(url.to_wire_json(), "serializable");
        assert_eq!(
            wire,
            r#"{"kind":"url","url":"https://example.com/v.mp4","media_type":"video/mp4"}"#
        );
        assert_eq!(ok(VideoSource::from_wire_json(&wire), "round trip"), url);

        // A decoded document is normalized and typed like a constructor's.
        let decoded = ok(
            ImageSource::from_wire_json(
                r#"{"kind":"inline","data":"YQ==","media_type":"IMAGE/PNG"}"#,
            ),
            "decodes",
        );
        assert_eq!(decoded.media_type(), "image/png");
        assert_eq!(
            AudioSource::from_wire_json(
                r#"{"kind":"inline","data":"YQ==","media_type":"image/png"}"#
            ),
            Err("media type \"image/png\" must use audio/".to_owned())
        );
    }

    #[test]
    fn the_wire_decode_refuses_what_core_refuses() {
        assert_eq!(
            ImageSource::from_wire_json(r#"{"kind":"stream","media_type":"image/png"}"#),
            Err("stream media source has invalid payload".to_owned())
        );
        assert_eq!(
            ImageSource::from_wire_json(r#"{"kind":"socket"}"#),
            Err("unknown media source kind \"socket\"".to_owned())
        );
        assert_eq!(
            ImageSource::from_wire_json(r#"{}"#),
            Err("unknown media source kind \"\"".to_owned())
        );
        assert_eq!(
            ImageSource::from_wire_json(r#"{"kind":"url","url":"https://x/y","extra":1}"#),
            Err("json: unknown field \"extra\"".to_owned())
        );
    }

    #[test]
    fn formats_validate_the_way_core_validates_them() {
        let raw = AudioFormat::new(AudioEncoding::Pcm16, Some(24_000), Some(1));
        assert_eq!(raw.validate(), Ok(()));
        assert_eq!(
            AudioFormat::new(AudioEncoding::Pcm16, None, None).validate(),
            Err("raw audio format requires sample rate and channels".to_owned())
        );
        // A zero is not a declaration: the raw side refuses it, the compressed
        // side reads it as absent.
        assert_eq!(
            AudioFormat::new(AudioEncoding::Pcm16, Some(0), Some(1)).validate(),
            Err("raw audio format requires sample rate and channels".to_owned())
        );
        assert_eq!(
            AudioFormat::new(AudioEncoding::Mp3, None, None).validate(),
            Ok(())
        );
        assert_eq!(
            AudioFormat::new(AudioEncoding::Mp3, Some(0), None).validate(),
            Ok(())
        );
        assert_eq!(
            AudioFormat::new(AudioEncoding::Mp3, Some(-1), None).validate(),
            Err("audio sample rate and channels must not be negative".to_owned())
        );
        assert_eq!(AudioEncoding::Pcm16.media_type(), "audio/pcm");
        assert_eq!(AudioEncoding::Mp3.media_type(), "audio/mpeg");
        assert_eq!(AudioEncoding::parse("pcm16"), Some(AudioEncoding::Pcm16));
        assert_eq!(AudioEncoding::parse("pcm32"), None);

        assert_eq!(ImageSize::new(1024, 768).validate(), Ok(()));
        assert_eq!(
            ImageSize::new(0, 768).validate(),
            Err("image dimensions must be positive".to_owned())
        );
        assert_eq!(AspectRatio::new("16:9").validate(), Ok(()));
        assert_eq!(
            AspectRatio::new("wide").validate(),
            Err("aspect ratio must use width:height".to_owned())
        );
        assert_eq!(
            AspectRatio::new("0:9").validate(),
            Err("aspect ratio components must be positive integers".to_owned())
        );
        assert_eq!(ImageFormat::Png.media_type(), "image/png");
        assert_eq!(ImageFormat::parse("bitmap"), None);
        assert_eq!(ImageQuality::XHigh.as_str(), "xhigh");
        assert_eq!(ImageQuality::parse("ultra"), None);

        let frame = VideoFrame {
            source: ok(ImageSource::new_inline(b"frame", "image/png"), "inline"),
            timestamp_millis: 40,
        };
        assert_eq!(frame.validate(), Ok(()));
        let late = VideoFrame {
            source: ok(ImageSource::new_inline(b"frame", "image/png"), "inline"),
            timestamp_millis: -1,
        };
        assert_eq!(
            late.validate(),
            Err("video frame timestamp must not be negative".to_owned())
        );
    }
}

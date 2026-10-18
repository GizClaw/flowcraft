//! `core/utils/filetype`: classify a file by its content, not its name.
//!
//! Hosts render, stage and archive files by their media type, so the name alone
//! is not enough evidence: macOS and Linux map `.ts` to `video/mp2t`, which
//! would stream TypeScript source into a `<video>` element, and no table tells a
//! screenshot saved as `.txt` from a note. The rule decides for every file,
//! whatever the platform's extension table says.
//!
//! Classification, in order:
//!
//! 1. A media signature in the leading bytes (image, video, audio, PDF) wins
//!    when the payload is binary *or* the name agrees on the family. The
//!    corroboration guards the two-letter patterns of the WHATWG table (`BM` is
//!    `image/bmp`, `ID3` is `audio/mpeg`), which ordinary documents can start
//!    with; the name keeps those text. A real image, video or container carries
//!    a NUL byte in the sample, while a PDF with a text preamble may not, so a
//!    `.pdf` still matches through its name.
//! 2. A payload without a NUL byte is text, whatever the name says. The name may
//!    only refine the subtype (`text/csv`, `text/markdown`).
//! 3. Anything else is binary and takes the name's type when the table has one
//!    (`.mkv`, `.heic`, `.docx`, `.zip` carry no signature the sample can
//!    name), and the sniffed type otherwise.
//!
//! Text has one definition here: a payload is text exactly when it has no NUL
//! byte. Encodings that embed NULs (UTF-16) therefore read as binary.
//!
//! # The extension table is Go's, including the part that comes from the host
//!
//! `NameType` is `mime.TypeByExtension`, and that is *not* a constant: Go starts
//! from its builtin table and then reads the host's MIME database -- the
//! FreeDesktop `globs2` file when it exists (where the builtin entries win),
//! otherwise `/etc/mime.types`, `/etc/apache2/mime.types`, `/etc/apache/mime.types`
//! and `/etc/httpd/conf/mime.types` in that order (where each file overrides the
//! one before it). On this port's macOS host that is why `.ts` is `video/mp2t`
//! and `.flac` is `audio/x-flac` (the Apache file), while `.md` is unknown.
//!
//! The port reads the same files in the same order, so the two answer alike on
//! the same machine -- and differently on different machines, exactly as core
//! does. That is also why the committed contract has no name-derived rows: a
//! snapshot of the resolved table would be a snapshot of one host. The parsers
//! are pure functions and are tested against fixture text instead.
//!
//! Two things Go's `setExtensionType` does and this does not: it *parses* each
//! entry's type and drops the entry when the type is malformed, and it appends
//! `; charset=utf-8` to a `text/*` entry with no charset. Neither is observable
//! through `NameType`, which trims the parameters off -- they would matter only
//! for a caller that asked the table for a full value.

use std::collections::HashMap;
use std::fs::File;
use std::io::{self, Read};
use std::path::Path;
use std::sync::OnceLock;

/// How many leading bytes the classifier samples. `net/http`'s
/// `DetectContentType` (the WHATWG mimesniff algorithm, section 5) looks at no
/// more than this.
pub const SNIFF_LEN: usize = 512;

/// How a host should treat one file: the media type to report for it and
/// whether its payload is text.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Kind {
    /// The type to report; it is never empty.
    pub media_type: String,
    /// The payload is text, so a viewer can render it as source and a tool can
    /// read it as lines.
    pub text: bool,
}

/// Classify the file at `path`. A path the process cannot read as a file (a
/// directory, a permission error) is classified by its name alone, with an
/// empty payload that reads as text.
pub fn of_path(path: impl AsRef<Path>) -> Kind {
    let path = path.as_ref();
    match read_head(path) {
        Ok(head) => classify(&path.to_string_lossy(), &head),
        Err(_) => classify(&path.to_string_lossy(), &[]),
    }
}

/// Classify a payload the caller already holds: a workspace file read through
/// the sandbox, a download a tool just fetched. `name` supplies the extension
/// and nothing else. The sample is the same [`SNIFF_LEN`] the path form reads,
/// so a file classifies identically either way.
pub fn of_data(name: &str, data: &[u8]) -> Kind {
    classify(name, &data[..data.len().min(SNIFF_LEN)])
}

/// Whether a payload is text under the rule the viewer, the attachments and the
/// tools share: text carries no NUL byte.
pub fn is_text(data: &[u8]) -> bool {
    !data.contains(&0)
}

/// The rendering family of a media type: `"image"`, `"video"`, `"audio"`,
/// `"pdf"`, or `""` for everything a host hands to an external application.
pub fn family(media_type: &str) -> &'static str {
    if media_type.starts_with("image/") {
        "image"
    } else if media_type.starts_with("video/") {
        "video"
    } else if media_type.starts_with("audio/") {
        "audio"
    } else if media_type == "application/pdf" {
        "pdf"
    } else {
        ""
    }
}

/// The MIME type the platform's extension table maps `name` to, trimmed of its
/// parameters, or `""` when the table does not know the extension.
pub fn name_type(name: &str) -> String {
    trimmed(lookup(&extension(name))).to_owned()
}

/// The ordered rule in the module docs.
fn classify(name: &str, head: &[u8]) -> Kind {
    let table = name_type(name);
    // core trims the sniffer's answer before it uses it, so a `Kind` never
    // carries the sniffer's `; charset=...` -- the UTF-16 marks included.
    let sample = trimmed(detect_content_type(head));
    let sample_family = family(sample);
    if !sample_family.is_empty() && (!is_text(head) || family(&table) == sample_family) {
        return Kind {
            media_type: sample.to_owned(),
            text: false,
        };
    }
    if is_text(head) {
        if table.starts_with("text/") {
            return Kind {
                media_type: table,
                text: true,
            };
        }
        return Kind {
            media_type: "text/plain".to_owned(),
            text: true,
        };
    }
    if !table.is_empty() && !table.starts_with("text/") {
        return Kind {
            media_type: table,
            text: false,
        };
    }
    // A binary payload no name claims, or one whose name claims text: the bytes
    // are as specific as the answer gets.
    Kind {
        media_type: sample.to_owned(),
        text: false,
    }
}

/// Sample the leading bytes the classifier reads. Short files return what they
/// have; a path that cannot be opened as a file (a directory) returns an error.
fn read_head(path: &Path) -> io::Result<Vec<u8>> {
    let mut file = File::open(path)?;
    let mut head = vec![0u8; SNIFF_LEN];
    let mut filled = 0;
    while filled < head.len() {
        match file.read(&mut head[filled..]) {
            Ok(0) => break,
            Ok(read) => filled += read,
            Err(err) if err.kind() == io::ErrorKind::Interrupted => continue,
            Err(err) => return Err(err),
        }
    }
    head.truncate(filled);
    Ok(head)
}

/// Drop the parameters a table entry attaches (`"text/plain; charset=utf-8"`).
fn trimmed(media_type: &str) -> &str {
    match media_type.split_once(';') {
        Some((base, _)) => base.trim(),
        None => media_type.trim(),
    }
}

/// `filepath.Ext`: the suffix beginning at the final dot of the final element,
/// `""` when there is none.
fn extension(name: &str) -> String {
    let file = match name.rfind('/') {
        Some(slash) => &name[slash + 1..],
        None => name,
    };
    match file.rfind('.') {
        Some(dot) => file[dot..].to_owned(),
        None => String::new(),
    }
}

/// `mime.TypeByExtension`: case-sensitive lookup, then case-insensitive.
fn lookup(ext: &str) -> &'static str {
    static TYPES: OnceLock<HashMap<String, String>> = OnceLock::new();
    let types = TYPES.get_or_init(resolved_types);
    let type_ = types.get(ext).or_else(|| types.get(&ext.to_lowercase()));
    match type_ {
        Some(type_) => type_.as_str(),
        None => "",
    }
}

/// Go's `builtinTypesLower`, in full: the table every host starts from, before
/// the host's own MIME database is read.
const BUILTIN: [(&str, &str); 65] = [
    (".ai", "application/postscript"),
    (".apk", "application/vnd.android.package-archive"),
    (".apng", "image/apng"),
    (".avif", "image/avif"),
    (".bin", "application/octet-stream"),
    (".bmp", "image/bmp"),
    (".com", "application/octet-stream"),
    (".css", "text/css; charset=utf-8"),
    (".csv", "text/csv; charset=utf-8"),
    (".doc", "application/msword"),
    (
        ".docx",
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    ),
    (".ehtml", "text/html; charset=utf-8"),
    (".eml", "message/rfc822"),
    (".eps", "application/postscript"),
    (".exe", "application/octet-stream"),
    (".flac", "audio/flac"),
    (".gif", "image/gif"),
    (".gz", "application/gzip"),
    (".htm", "text/html; charset=utf-8"),
    (".html", "text/html; charset=utf-8"),
    (".ico", "image/vnd.microsoft.icon"),
    (".ics", "text/calendar; charset=utf-8"),
    (".jfif", "image/jpeg"),
    (".jpeg", "image/jpeg"),
    (".jpg", "image/jpeg"),
    (".js", "text/javascript; charset=utf-8"),
    (".json", "application/json"),
    (".m4a", "audio/mp4"),
    (".mjs", "text/javascript; charset=utf-8"),
    (".mp3", "audio/mpeg"),
    (".mp4", "video/mp4"),
    (".oga", "audio/ogg"),
    (".ogg", "audio/ogg"),
    (".ogv", "video/ogg"),
    (".opus", "audio/ogg"),
    (".pdf", "application/pdf"),
    (".pjp", "image/jpeg"),
    (".pjpeg", "image/jpeg"),
    (".png", "image/png"),
    (".ppt", "application/vnd.ms-powerpoint"),
    (
        ".pptx",
        "application/vnd.openxmlformats-officedocument.presentationml.presentation",
    ),
    (".ps", "application/postscript"),
    (".rdf", "application/rdf+xml"),
    (".rtf", "application/rtf"),
    (".shtml", "text/html; charset=utf-8"),
    (".svg", "image/svg+xml"),
    (".text", "text/plain; charset=utf-8"),
    (".tif", "image/tiff"),
    (".tiff", "image/tiff"),
    (".txt", "text/plain; charset=utf-8"),
    (".vtt", "text/vtt; charset=utf-8"),
    (".wasm", "application/wasm"),
    (".wav", "audio/wav"),
    (".weba", "audio/webm"),
    (".webm", "video/webm"),
    (".webp", "image/webp"),
    (".xbl", "text/xml; charset=utf-8"),
    (".xbm", "image/x-xbitmap"),
    (".xht", "application/xhtml+xml"),
    (".xhtml", "application/xhtml+xml"),
    (".xls", "application/vnd.ms-excel"),
    (
        ".xlsx",
        "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    ),
    (".xml", "text/xml; charset=utf-8"),
    (".xsl", "text/xml; charset=utf-8"),
    (".zip", "application/zip"),
];

/// The FreeDesktop databases Go prefers, in its order.
const MIME_GLOBS: [&str; 2] = ["/usr/local/share/mime/globs2", "/usr/share/mime/globs2"];

/// The per-platform `mime.types` files Go falls back to, in its order.
const TYPE_FILES: [&str; 4] = [
    "/etc/mime.types",
    "/etc/apache2/mime.types",
    "/etc/apache/mime.types",
    "/etc/httpd/conf/mime.types",
];

/// The builtin table with the host's database applied, the way `initMimeUnix`
/// applies it.
fn resolved_types() -> HashMap<String, String> {
    let mut types: HashMap<String, String> = BUILTIN
        .iter()
        .map(|(ext, type_)| ((*ext).to_owned(), (*type_).to_owned()))
        .collect();
    // The globs2 database, when there is one, is the whole overlay: Go stops at
    // the first file it can open and never reads the type files.
    for path in MIME_GLOBS {
        if let Ok(text) = std::fs::read_to_string(path) {
            merge_globs2(&text, &mut types);
            return types;
        }
    }
    for path in TYPE_FILES {
        if let Ok(text) = std::fs::read_to_string(path) {
            merge_mime_types(&text, &mut types);
        }
    }
    types
}

/// One `globs2` line: `weight:mimetype:glob[:more]`. Only a bare `*.ext` glob is
/// taken, and the first entry for an extension wins -- the file is in weight
/// order, so the heaviest stays.
fn merge_globs2(text: &str, types: &mut HashMap<String, String>) {
    for line in text.lines() {
        let fields: Vec<&str> = line.split(':').collect();
        if fields.len() < 3 || fields[0].is_empty() || fields[2].len() < 3 {
            continue;
        }
        if fields[0].starts_with('#') || !fields[2].starts_with("*.") {
            continue;
        }
        let extension = &fields[2][1..];
        if extension.contains(['?', '*', '[']) {
            // Not a bare extension but a glob, which Go ignores too.
            continue;
        }
        types
            .entry(extension.to_owned())
            .or_insert_with(|| fields[1].to_owned());
    }
}

/// One `mime.types` line: a type, then the extensions it claims. Unlike the
/// globs database, these entries override what is already known -- this is the
/// file that makes `.flac` `audio/x-flac` on macOS.
fn merge_mime_types(text: &str, types: &mut HashMap<String, String>) {
    for line in text.lines() {
        let mut fields = line.split_whitespace();
        let Some(type_) = fields.next() else {
            continue;
        };
        // A line that is only a type names no extension.
        let extensions: Vec<&str> = fields.collect();
        if type_.starts_with('#') || extensions.is_empty() {
            continue;
        }
        for extension in extensions {
            if extension.starts_with('#') {
                break;
            }
            types.insert(format!(".{extension}"), type_.to_owned());
        }
    }
}

/// `net/http`'s `DetectContentType`: the WHATWG mimesniff algorithm over the
/// first [`SNIFF_LEN`] bytes. Always answers a valid type; the fallback is
/// `application/octet-stream`.
pub fn detect_content_type(data: &[u8]) -> &'static str {
    let data = &data[..data.len().min(SNIFF_LEN)];
    let first_non_ws = data.iter().take_while(|byte| is_ws(**byte)).count();
    for signature in &SIGNATURES {
        if let Some(type_) = signature.matches(data, first_non_ws) {
            return type_;
        }
    }
    "application/octet-stream"
}

/// A whitespace byte as the spec defines 0xWS.
fn is_ws(byte: u8) -> bool {
    matches!(byte, b'\t' | b'\n' | 0x0c | b'\r' | b' ')
}

/// A tag-terminating byte as the spec defines 0xTT.
fn is_tt(byte: u8) -> bool {
    matches!(byte, b' ' | b'>')
}

/// One row of `sniffSignatures`, in Go's order -- the order is the algorithm.
enum Signature {
    /// `exactSig`: the payload starts with these bytes.
    Exact {
        pattern: &'static [u8],
        type_: &'static str,
    },
    /// `maskedSig`: `payload[i] & mask[i] == pattern[i]`. A mismatched length is
    /// a non-match, as in Go.
    Masked {
        mask: &'static [u8],
        pattern: &'static [u8],
        skip_ws: bool,
        type_: &'static str,
    },
    /// `htmlSig`: a case-insensitive tag name, terminated by 0xTT.
    Html(&'static [u8]),
    /// `mp4Sig`: the `ftyp` box walk.
    Mp4,
    /// `textSig`: text is the absence of control bytes, and it comes last.
    Text,
}

impl Signature {
    fn matches(&self, data: &[u8], first_non_ws: usize) -> Option<&'static str> {
        match self {
            Signature::Exact { pattern, type_ } => data.starts_with(pattern).then_some(*type_),
            Signature::Masked {
                mask,
                pattern,
                skip_ws,
                type_,
            } => {
                let data = if *skip_ws {
                    &data[first_non_ws..]
                } else {
                    data
                };
                if mask.len() != pattern.len() || data.len() < pattern.len() {
                    return None;
                }
                let matched = pattern
                    .iter()
                    .zip(mask.iter())
                    .zip(data.iter())
                    .all(|((pattern, mask), byte)| byte & mask == *pattern);
                matched.then_some(*type_)
            }
            Signature::Html(tag) => {
                let data = &data[first_non_ws..];
                if data.len() < tag.len() + 1 {
                    return None;
                }
                let head_matches = tag.iter().zip(data.iter()).all(|(tag, byte)| {
                    let folded = if tag.is_ascii_uppercase() {
                        byte & 0xDF
                    } else {
                        *byte
                    };
                    *tag == folded
                });
                (head_matches && is_tt(data[tag.len()])).then_some("text/html; charset=utf-8")
            }
            Signature::Mp4 => mp4_type(data),
            Signature::Text => {
                let printable = data[first_non_ws..]
                    .iter()
                    .all(|byte| !matches!(*byte, 0x00..=0x08 | 0x0b | 0x0e..=0x1a | 0x1c..=0x1f));
                printable.then_some("text/plain; charset=utf-8")
            }
        }
    }
}

/// The MP4 box walk: a box that fits the sample, whose type is `ftyp`, and one
/// of whose four-byte fields (skipping the major brand's version) starts with
/// `mp4`.
fn mp4_type(data: &[u8]) -> Option<&'static str> {
    if data.len() < 12 {
        return None;
    }
    let box_size = u32::from_be_bytes([data[0], data[1], data[2], data[3]]) as usize;
    if data.len() < box_size || !box_size.is_multiple_of(4) {
        return None;
    }
    if &data[4..8] != b"ftyp" {
        return None;
    }
    let mut field = 8;
    while field < box_size {
        if field == 12 {
            // The four bytes of the major brand's version number.
            field += 4;
            continue;
        }
        if data[field..field + 3] == *b"mp4" {
            return Some("video/mp4");
        }
        field += 4;
    }
    None
}

/// Go's `sniffSignatures`, transcribed in order.
#[rustfmt::skip]
static SIGNATURES: [Signature; 51] = [
    Signature::Html(b"<!DOCTYPE HTML"),
    Signature::Html(b"<HTML"),
    Signature::Html(b"<HEAD"),
    Signature::Html(b"<SCRIPT"),
    Signature::Html(b"<IFRAME"),
    Signature::Html(b"<H1"),
    Signature::Html(b"<DIV"),
    Signature::Html(b"<FONT"),
    Signature::Html(b"<TABLE"),
    Signature::Html(b"<A"),
    Signature::Html(b"<STYLE"),
    Signature::Html(b"<TITLE"),
    Signature::Html(b"<B"),
    Signature::Html(b"<BODY"),
    Signature::Html(b"<BR"),
    Signature::Html(b"<P"),
    Signature::Html(b"<!--"),
    Signature::Masked { mask: b"\xFF\xFF\xFF\xFF\xFF", pattern: b"<?xml", skip_ws: true, type_: "text/xml; charset=utf-8" },
    Signature::Exact { pattern: b"%PDF-", type_: "application/pdf" },
    Signature::Exact { pattern: b"%!PS-Adobe-", type_: "application/postscript" },
    // UTF byte-order marks.
    Signature::Masked { mask: b"\xFF\xFF\x00\x00", pattern: b"\xFE\xFF\x00\x00", skip_ws: false, type_: "text/plain; charset=utf-16be" },
    Signature::Masked { mask: b"\xFF\xFF\x00\x00", pattern: b"\xFF\xFE\x00\x00", skip_ws: false, type_: "text/plain; charset=utf-16le" },
    Signature::Masked { mask: b"\xFF\xFF\xFF\x00", pattern: b"\xEF\xBB\xBF\x00", skip_ws: false, type_: "text/plain; charset=utf-8" },
    // Images.
    Signature::Exact { pattern: b"\x00\x00\x01\x00", type_: "image/x-icon" },
    Signature::Exact { pattern: b"\x00\x00\x02\x00", type_: "image/x-icon" },
    Signature::Exact { pattern: b"BM", type_: "image/bmp" },
    Signature::Exact { pattern: b"GIF87a", type_: "image/gif" },
    Signature::Exact { pattern: b"GIF89a", type_: "image/gif" },
    Signature::Masked { mask: b"\xFF\xFF\xFF\xFF\x00\x00\x00\x00\xFF\xFF\xFF\xFF\xFF\xFF", pattern: b"RIFF\x00\x00\x00\x00WEBPVP", skip_ws: false, type_: "image/webp" },
    Signature::Exact { pattern: b"\x89PNG\x0D\x0A\x1A\x0A", type_: "image/png" },
    Signature::Exact { pattern: b"\xFF\xD8\xFF", type_: "image/jpeg" },
    // Audio and video, in the order the spec prescribes.
    Signature::Masked { mask: b"\xFF\xFF\xFF\xFF\x00\x00\x00\x00\xFF\xFF\xFF\xFF", pattern: b"FORM\x00\x00\x00\x00AIFF", skip_ws: false, type_: "audio/aiff" },
    Signature::Masked { mask: b"\xFF\xFF\xFF", pattern: b"ID3", skip_ws: false, type_: "audio/mpeg" },
    Signature::Masked { mask: b"\xFF\xFF\xFF\xFF\xFF", pattern: b"OggS\x00", skip_ws: false, type_: "application/ogg" },
    Signature::Masked { mask: b"\xFF\xFF\xFF\xFF\xFF\xFF\xFF\xFF", pattern: b"MThd\x00\x00\x00\x06", skip_ws: false, type_: "audio/midi" },
    Signature::Masked { mask: b"\xFF\xFF\xFF\xFF\x00\x00\x00\x00\xFF\xFF\xFF\xFF", pattern: b"RIFF\x00\x00\x00\x00AVI ", skip_ws: false, type_: "video/avi" },
    Signature::Masked { mask: b"\xFF\xFF\xFF\xFF\x00\x00\x00\x00\xFF\xFF\xFF\xFF", pattern: b"RIFF\x00\x00\x00\x00WAVE", skip_ws: false, type_: "audio/wave" },
    Signature::Mp4,
    Signature::Exact { pattern: b"\x1A\x45\xDF\xA3", type_: "video/webm" },
    // Fonts. 34 NUL bytes, then "LP" under a two-byte mask.
    Signature::Masked {
        mask: b"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xFF\xFF",
        pattern: b"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00LP",
        skip_ws: false,
        type_: "application/vnd.ms-fontobject",
    },
    Signature::Exact { pattern: b"\x00\x01\x00\x00", type_: "font/ttf" },
    Signature::Exact { pattern: b"OTTO", type_: "font/otf" },
    Signature::Exact { pattern: b"ttcf", type_: "font/collection" },
    Signature::Exact { pattern: b"wOFF", type_: "font/woff" },
    Signature::Exact { pattern: b"wOF2", type_: "font/woff2" },
    // Archives.
    Signature::Exact { pattern: b"\x1F\x8B\x08", type_: "application/x-gzip" },
    Signature::Exact { pattern: b"PK\x03\x04", type_: "application/zip" },
    Signature::Exact { pattern: b"Rar!\x1A\x07\x00", type_: "application/x-rar-compressed" },
    Signature::Exact { pattern: b"Rar!\x1A\x07\x01\x00", type_: "application/x-rar-compressed" },
    Signature::Exact { pattern: b"\x00\x61\x73\x6D", type_: "application/wasm" },
    // Text last: it matches nearly everything that is not binary.
    Signature::Text,
];

#[cfg(test)]
mod tests {
    use super::*;

    /// A PNG header: the signature carries no NUL, the IHDR chunk's dimensions
    /// do -- which is why a picture is still binary.
    fn png() -> Vec<u8> {
        vec![
            0x89, b'P', b'N', b'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, b'I', b'H',
            b'D', b'R', 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x02, 0x08, 0x06, 0x00, 0x00,
            0x00,
        ]
    }

    fn jpeg() -> Vec<u8> {
        let mut data = vec![
            0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, b'J', b'F', b'I', b'F', 0x00,
        ];
        data.extend(std::iter::repeat_n(0x11u8, 16));
        data
    }

    /// The ftyp box the WHATWG sniffer matches, so the content names
    /// `video/mp4` on every platform.
    fn mp4() -> Vec<u8> {
        let mut data = vec![0x00, 0x00, 0x00, 0x20];
        data.extend_from_slice(b"ftypisom");
        data.extend_from_slice(&[0x00, 0x00, 0x00, 0x00]);
        data.extend_from_slice(b"mp41");
        data.extend(std::iter::repeat_n(0x42u8, 32));
        data
    }

    /// One PAT packet of an MPEG transport stream: sync byte, PID 0, table_id
    /// 0, then NUL padding -- what a `.ts` recording is.
    fn ts_packet() -> Vec<u8> {
        let mut data = vec![0x47, 0x40, 0x00, 0x10, 0x00];
        data.extend(std::iter::repeat_n(0u8, 183));
        data
    }

    /// core's `TestOfPathDecidesByContentNotName`, one case at a time: the
    /// bytes decide the type and the name may only refine it.
    #[test]
    fn content_decides_and_the_name_refines() {
        let source = b"export const answer = 42;\n";
        // The case that motivated the rule: the table maps .ts to video/mp2t,
        // the bytes are TypeScript.
        let kind = of_data("app.ts", source);
        assert!(kind.text, "{kind:?}");
        assert_eq!(kind.media_type, "text/plain");

        // A video name cannot make text a video.
        let kind = of_data("clip.mp4", b"not a video at all\n");
        assert!(kind.text);
        assert_eq!(kind.media_type, "text/plain");

        // A text name cannot hide a picture: the bytes win.
        assert_eq!(of_data("screenshot.txt", &png()).media_type, "image/png");
        // Nor a video.
        assert_eq!(of_data("notes.md", &mp4()).media_type, "video/mp4");
        // A wrong media name cannot beat the bytes either.
        assert_eq!(of_data("photo.png", &jpeg()).media_type, "image/jpeg");

        // Two-letter WHATWG patterns that documents start with.
        for (name, text) in [
            ("bm25.md", "BM25 ranks documents by term frequency.\n"),
            ("tags.md", "ID3 tags carry track metadata.\n"),
        ] {
            let kind = of_data(name, text.as_bytes());
            assert!(kind.text, "{name}: {kind:?}");
            assert_eq!(kind.media_type, "text/plain");
        }
        // The pattern is real when the bytes carry it.
        let mut bmp = b"BM".to_vec();
        bmp.extend(std::iter::repeat_n(0u8, 56));
        assert_eq!(of_data("icon.bmp", &bmp).media_type, "image/bmp");

        // A text-only PDF preamble has no NUL for the sniffer to lean on, so
        // the name corroborates the signature.
        assert_eq!(
            of_data("report.pdf", b"%PDF-1.4 fake\n").media_type,
            "application/pdf"
        );

        // Text under a name the table calls an image opens as text.
        let kind = of_data("diagram.svg", b"<svg viewBox=\"0 0 1 1\"></svg>\n");
        assert!(kind.text);

        // An archive whose exact format only the name knows.
        let mut zip = b"PK\x03\x04".to_vec();
        zip.extend(std::iter::repeat_n(0u8, 24));
        assert_eq!(
            of_data("report.docx", &zip).media_type,
            name_type("report.docx"),
            "a binary with a name the table knows takes the name's type"
        );
    }

    #[test]
    fn the_rule_that_needs_no_table() {
        // No extension: the bytes decide between text and the generic binary
        // fallback.
        assert!(of_data("LICENSE", b"MIT License\n").text);
        assert_eq!(
            of_data("blob", &[0x00, 0x01, 0x02, 0x03]).media_type,
            "application/octet-stream"
        );
        // A NUL rules text out even under a text name.
        assert!(!of_data("broken.md", &[0x00, 0x01, 0x02, 0x7F]).text);
        // An empty file is text.
        assert!(of_data("empty.go", b"").text);
        assert_eq!(of_data("empty.go", b"").media_type, "text/plain");
        // The same extension, opposite verdicts.
        assert!(of_data("app.ts", b"export const answer = 42;\n").text);
        let recording = of_data("recording.ts", &ts_packet());
        assert!(!recording.text);
        // Which binary type that is follows the table: video/mp2t where the
        // host knows .ts, the sniffer's answer otherwise.
        let named = name_type("recording.ts");
        assert_eq!(
            recording.media_type,
            if named.is_empty() {
                "application/octet-stream".to_owned()
            } else {
                named
            }
        );
    }

    #[test]
    fn kind_carries_no_parameters() {
        // core trims both halves, so a Kind never prints a charset -- not even
        // for the UTF-16 byte-order marks, whose payload is binary.
        let kind = of_data("blob", &[0xFE, 0xFF, 0x00, 0x00]);
        assert_eq!(kind.media_type, "text/plain");
        assert!(!kind.text);
        assert_eq!(name_type("note.txt"), "text/plain");
        assert_eq!(name_type("doc.unknown-extension"), "");
    }

    /// `IsText` is one question -- is there a NUL? -- and it is *not* the
    /// sniffer's text signature, which also refuses control bytes. The
    /// difference is visible: a lone 0x08 is text here and
    /// `application/octet-stream` there.
    #[test]
    fn is_text_is_one_question() {
        assert!(is_text(b""));
        assert!(is_text(b"plain text\n"));
        assert!(!is_text(b"head\x00tail"));
        for byte in [0x08u8, 0x0b, 0x0e, 0x1a, 0x1c, 0x1f] {
            assert!(is_text(&[byte]), "{byte:#04x}");
            assert_eq!(detect_content_type(&[byte]), "application/octet-stream");
            assert_eq!(of_data("blob", &[byte]).media_type, "text/plain");
        }
    }

    #[test]
    fn family_is_the_rendering_hint() {
        for (media_type, family_) in [
            ("image/png", "image"),
            ("video/mp2t", "video"),
            ("audio/mpeg", "audio"),
            ("application/pdf", "pdf"),
            ("text/plain", ""),
            ("application/zip", ""),
            ("application/pdf+xml?", ""),
        ] {
            assert_eq!(family(media_type), family_, "{media_type}");
        }
    }

    /// A path that is not a readable file still classifies, so callers listing a
    /// directory do not need a special case.
    #[test]
    fn a_path_without_readable_content_falls_back_to_the_name() -> std::io::Result<()> {
        let dir = std::env::temp_dir().join(format!("filetype-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir)?;
        assert_eq!(family(&of_path(&dir).media_type), "");
        let missing = of_path(dir.join("missing.txt"));
        assert!(missing.text, "{missing:?}");
        assert_eq!(missing.media_type, "text/plain");
        let _ = std::fs::remove_dir_all(&dir);
        Ok(())
    }

    /// The in-memory form answers for the same bytes the way the path form does,
    /// so a tool that already holds a payload classifies files exactly like the
    /// viewer.
    #[test]
    fn of_data_matches_of_path() -> std::io::Result<()> {
        let dir = std::env::temp_dir().join(format!("filetype-data-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir)?;
        for (name, data) in [
            ("app.ts", b"export const answer = 42;\n".to_vec()),
            ("clip.mp4", b"not a video at all\n".to_vec()),
            ("screenshot.txt", png()),
            ("recording.ts", ts_packet()),
            ("blob", vec![0x00, 0x01, 0x02]),
        ] {
            let path = dir.join(name);
            std::fs::write(&path, &data)?;
            assert_eq!(of_data(name, &data), of_path(&path), "{name}");
        }
        let _ = std::fs::remove_dir_all(&dir);
        Ok(())
    }

    #[test]
    fn the_builtin_table_is_go_s() {
        assert_eq!(BUILTIN.len(), 65);
        for (extension, type_) in BUILTIN {
            assert!(extension.starts_with('.'), "{extension}");
            assert_eq!(extension, &extension.to_lowercase(), "{extension}");
            assert!(!type_.is_empty(), "{extension}");
        }
        assert_eq!(name_type("index.html"), "text/html");
        assert_eq!(name_type("a.zip"), "application/zip");
    }

    /// The overlay's precedence, on fixture text: the globs database fills only
    /// what the builtin table does not know, while `mime.types` overrides.
    #[test]
    fn the_parsers_follow_the_precedence() {
        let mut types: HashMap<String, String> = BUILTIN
            .iter()
            .map(|(ext, type_)| ((*ext).to_owned(), (*type_).to_owned()))
            .collect();

        let globs = "\
# comment line
50:image/png:*.png
10:video/mp2t:*.ts
10:text/x-note:*.note
10:text/x-glob:*.tar.*
10:text/x-question:*.x?
";
        merge_globs2(globs, &mut types);
        assert_eq!(
            types[".png"], "image/png",
            "a known extension is not replaced"
        );
        assert_eq!(types[".ts"], "video/mp2t", "an unknown one is filled in");
        assert_eq!(types[".note"], "text/x-note");
        assert!(
            !types.contains_key(".tar.*"),
            "a glob with metacharacters is ignored"
        );
        assert!(!types.contains_key(".x?"));
        // First entry wins, which is what the weight order is for.
        merge_globs2("5:text/x-other:*.ts\n", &mut types);
        assert_eq!(types[".ts"], "video/mp2t");

        let mime_types = "\
video/mp2t ts m2t mts
audio/x-flac flac
# a comment
text/x-ignored
text/plain txt # a trailing comment drops the rest
";
        merge_mime_types(mime_types, &mut types);
        assert_eq!(
            types[".ts"], "video/mp2t",
            "this file overrides, and the same value is not a change"
        );
        assert_eq!(types[".flac"], "audio/x-flac", "an override wins");
        assert_eq!(types[".mts"], "video/mp2t");
        assert_eq!(types[".txt"], "text/plain");
        assert!(!types.contains_key(".ignored"));
    }

    /// The host half of the table, measured rather than assumed: where a
    /// `globs2` or `mime.types` file exists, both it and core know `.ts` -- the
    /// extension the package exists for -- as `video/mp2t`.
    #[test]
    fn the_host_table_still_speaks_for_ts() {
        let globs = MIME_GLOBS
            .iter()
            .any(|path| std::path::Path::new(path).exists());
        let types = TYPE_FILES
            .iter()
            .any(|path| std::path::Path::new(path).exists());
        if !globs && !types {
            // A host with no MIME database: the builtin table is all there is,
            // and it does not know .ts.
            assert_eq!(name_type("x.ts"), "");
            return;
        }
        assert_eq!(name_type("x.ts"), "video/mp2t");
    }
}

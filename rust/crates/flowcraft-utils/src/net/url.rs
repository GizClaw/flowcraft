//! `net/url.Parse`, for the one grammar the proxy settings are read with.
//!
//! [`NetPolicy::proxy`](super::policy::NetPolicy::proxy) is a URL, and core
//! decides whether it is usable by parsing it and looking at three fields: the
//! scheme, the host, and whether the userinfo carries a password without a
//! username. Everything that can make `url.Parse` refuse -- a control byte, a
//! malformed `%`-escape, a bracketed host that is not an IP literal, a port
//! that is not digits -- is therefore part of the validation, and reproducing
//! the *answer* means reproducing the grammar.
//!
//! This is that grammar, and only that grammar: [`parse`] returns the fields
//! core's validator reads, plus the ones the contract can observe (path,
//! query, fragment, opaque). It is not a URL library. There is no
//! serialization, no resolution against a base, no `RawPath` bookkeeping, and
//! no query unescaping -- core does not do those here either, at parse time.
//!
//! # Deliberate limits
//!
//! - The strict-colon rule is the one Go 1.27 applies by default: an `http` or
//!   `https` host with more than one colon is refused, where `socks5` reads the
//!   last colon as the port separator. The `urlstrictcolons` GODEBUG setting
//!   can turn that off in Go; the port follows the default.
//! - A `%`-escape that decodes to bytes which are not UTF-8 cannot be
//!   represented: Go's `Path` holds the bytes, this `path` holds U+FFFD for
//!   each byte that does not start a valid sequence. Only an unprintable path
//!   is affected, and the JSON the contract is written in cannot spell those
//!   bytes either.
//! - `ParseRequestURI` is not here. Nothing in the proxy reads a request URI
//!   through this grammar; the HTTP slice parses the request line as HTTP.
//! - `ForceQuery` and `Fragment` are parsed because the grammar reaches them
//!   (a trailing `?` changes where the query is cut, and a fragment is
//!   unescaped after the main parse, which is the one way an invalid escape
//!   after `#` is refused). `OmitHost` is not: nothing reads it.

use flowcraft_contract::text;

use super::addr::Addr;

/// A parsed userinfo: the username, and the password if one was written.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Userinfo {
    /// The username, `%`-unescaped.
    pub username: String,
    /// The password, `%`-unescaped. `None` when the userinfo had no `:`.
    ///
    /// An empty password (`user:@host`) is `Some("")`, which is what makes
    /// core's "password but no username" check reachable.
    pub password: Option<String>,
}

/// The fields of a parsed URL this slice needs.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Url {
    /// The scheme, lowercased.
    pub scheme: String,
    /// The opaque part, for a scheme-relative reference (`mailto:x`).
    pub opaque: String,
    /// The userinfo, if the authority carried one.
    pub user: Option<Userinfo>,
    /// The host, including any port and, for an IP literal, its brackets.
    pub host: String,
    /// The path, `%`-unescaped.
    pub path: String,
    /// The query, exactly as written (core does not unescape it at parse time).
    pub raw_query: String,
    /// Whether the URL ended with a lone `?`.
    pub force_query: bool,
    /// The fragment, `%`-unescaped.
    pub fragment: String,
}

/// A refusal, spelled the way core spells it: `parse "<url>": <reason>`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParseError {
    /// The URL the message quotes: the input without its fragment for a
    /// parse-time refusal, and the whole input for a fragment one -- core
    /// wraps the two at different points, and the message shows it.
    url: String,
    /// The reason, without the `parse "..."` wrapper.
    message: String,
}

impl std::fmt::Display for ParseError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "parse {}: {}", text::quote(&self.url), self.message)
    }
}

impl std::error::Error for ParseError {}

/// The component being unescaped, which decides what `%`-escapes are legal and
/// which raw bytes are.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Mode {
    /// A host, where an escaped ASCII byte is refused: `%`-encoding may only
    /// introduce non-ASCII, plus `%25` for an IPv6 zone.
    Host,
    /// An IPv6 zone, which may use anything a host may plus a space.
    Zone,
    /// Userinfo.
    UserPassword,
    /// A path.
    Path,
    /// A fragment.
    Fragment,
}

/// `url.Parse`.
///
/// The message is core's, ready to be wrapped by the caller: `parse "<url>":
/// <reason>`.
pub fn parse(raw: &str) -> Result<Url, ParseError> {
    // The fragment is cut first and unescaped last, so an invalid escape
    // after `#` is quoted with the whole input, not with the part before it.
    let (head, fragment) = match raw.split_once('#') {
        Some((head, fragment)) => (head, Some(fragment)),
        None => (raw, None),
    };
    let mut url = parse_reference(head).map_err(|message| ParseError {
        url: head.to_owned(),
        message,
    })?;
    if let Some(fragment) = fragment.filter(|fragment| !fragment.is_empty()) {
        url.fragment = unescape(fragment, Mode::Fragment).map_err(|message| ParseError {
            url: raw.to_owned(),
            message,
        })?;
    }
    Ok(url)
}

/// The parse of everything before the fragment.
fn parse_reference(raw: &str) -> Result<Url, String> {
    if raw.bytes().any(|byte| byte < 0x20 || byte == 0x7f) {
        return Err("net/url: invalid control character in URL".to_owned());
    }
    let mut url = Url::default();
    if raw == "*" {
        url.path = "*".to_owned();
        return Ok(url);
    }

    let (scheme, mut rest) = get_scheme(raw)?;
    url.scheme = scheme.to_ascii_lowercase();

    if rest.ends_with('?') && rest.matches('?').count() == 1 {
        url.force_query = true;
        rest = &rest[..rest.len() - 1];
    } else if let Some((head, query)) = rest.split_once('?') {
        rest = head;
        url.raw_query = query.to_owned();
    }

    if !rest.starts_with('/') {
        if !url.scheme.is_empty() {
            // RFC 3986 calls a rootless path after a scheme opaque.
            url.opaque = rest.to_owned();
            return Ok(url);
        }
        // A relative reference may not carry a colon in its first segment:
        // `cache_object:foo/bar` is a scheme nobody registered, not a path.
        let segment = rest.split('/').next().unwrap_or_default();
        if segment.contains(':') {
            return Err("first path segment in URL cannot contain colon".to_owned());
        }
    }

    if (!url.scheme.is_empty() || !rest.starts_with("///")) && rest.starts_with("//") {
        let after_slashes = &rest[2..];
        let (authority, tail) = match after_slashes.find('/') {
            Some(index) => (&after_slashes[..index], &after_slashes[index..]),
            None => (after_slashes, ""),
        };
        let (user, host) = parse_authority(&url.scheme, authority)?;
        url.user = user;
        url.host = host;
        rest = tail;
    }

    url.path = unescape(rest, Mode::Path)?;
    Ok(url)
}

/// `getScheme`: everything before the first `:`, if that is a scheme.
fn get_scheme(raw: &str) -> Result<(&str, &str), String> {
    for (index, byte) in raw.bytes().enumerate() {
        match byte {
            b'a'..=b'z' | b'A'..=b'Z' => {}
            b'0'..=b'9' | b'+' | b'-' | b'.' => {
                if index == 0 {
                    return Ok(("", raw));
                }
            }
            b':' => {
                if index == 0 {
                    return Err("missing protocol scheme".to_owned());
                }
                return Ok((&raw[..index], &raw[index + 1..]));
            }
            _ => return Ok(("", raw)),
        }
    }
    Ok(("", raw))
}

fn parse_authority(scheme: &str, authority: &str) -> Result<(Option<Userinfo>, String), String> {
    let (userinfo, host_part) = match authority.rfind('@') {
        Some(index) => (Some(&authority[..index]), &authority[index + 1..]),
        None => (None, authority),
    };
    let host = parse_host(scheme, host_part)?;
    let Some(userinfo) = userinfo else {
        return Ok((None, host));
    };
    if !valid_userinfo(userinfo) {
        return Err("net/url: invalid userinfo".to_owned());
    }
    match userinfo.split_once(':') {
        None => Ok((
            Some(Userinfo {
                username: unescape(userinfo, Mode::UserPassword)?,
                password: None,
            }),
            host,
        )),
        Some((username, password)) => Ok((
            Some(Userinfo {
                username: unescape(username, Mode::UserPassword)?,
                password: Some(unescape(password, Mode::UserPassword)?),
            }),
            host,
        )),
    }
}

/// `parseHost`: an authority without userinfo.
fn parse_host(scheme: &str, host: &str) -> Result<String, String> {
    match host.rfind('[') {
        Some(open) if open > 0 => return Err("invalid IP-literal".to_owned()),
        Some(open) => {
            debug_assert_eq!(open, 0);
            let close = match host.rfind(']') {
                Some(close) => close,
                None => return Err("missing ']' in host".to_owned()),
            };
            let colon_port = &host[close + 1..];
            if !valid_optional_port(colon_port) {
                return Err(format!(
                    "invalid port {} after host",
                    text::quote(colon_port)
                ));
            }
            let unescaped_colon_port = unescape(colon_port, Mode::Host)?;
            let hostname = &host[1..close];
            // RFC 6874: `%25` introduces the zone, and the zone may use
            // `%`-encodings the host part may not.
            let unescaped_hostname = match hostname.find("%25") {
                Some(zone) => {
                    let host_part = unescape(&hostname[..zone], Mode::Host)?;
                    let zone_part = unescape(&hostname[zone..], Mode::Zone)?;
                    format!("{host_part}{zone_part}")
                }
                None => unescape(hostname, Mode::Host)?,
            };
            match Addr::parse(&unescaped_hostname) {
                Some(Addr::V6 { .. }) => {}
                // Brackets are for IPv6 only, and notably not for an
                // IPv4-mapped address, which `Addr::parse` keeps as V6.
                Some(Addr::V4(_)) => return Err("invalid IP-literal".to_owned()),
                None => {
                    return Err(format!(
                        "invalid host: ParseAddr({}): {}",
                        text::quote(&unescaped_hostname),
                        netip_reason(&unescaped_hostname)
                    ));
                }
            }
            return Ok(format!("[{unescaped_hostname}]{unescaped_colon_port}"));
        }
        None => {}
    }

    if let Some(first_colon) = host.find(':') {
        let last_colon = host.rfind(':').unwrap_or(first_colon);
        // Strict colons for the two schemes that go through authority
        // parsing most often; every other scheme reads the last colon as
        // the port separator. A host with several colons then fails the
        // port check only if the tail is not digits.
        let colon = if last_colon == first_colon || scheme == "http" || scheme == "https" {
            first_colon
        } else {
            last_colon
        };
        let colon_port = &host[colon..];
        if !valid_optional_port(colon_port) {
            return Err(format!(
                "invalid port {} after host",
                text::quote(colon_port)
            ));
        }
    }

    unescape(host, Mode::Host)
}

/// Why `Addr::parse` refused the literal, in `netip`'s words for the two
/// shapes this grammar can hand it.
fn netip_reason(text: &str) -> &'static str {
    match text.split_once('%') {
        Some((_, "")) => "zone must be a non-empty string",
        _ => "unable to parse IP",
    }
}

fn valid_optional_port(port: &str) -> bool {
    match port.strip_prefix(':') {
        None => port.is_empty(),
        Some(digits) => digits.bytes().all(|byte| byte.is_ascii_digit()),
    }
}

/// `validUserinfo`: the characters RFC 3986 allows before the `@`.
fn valid_userinfo(text: &str) -> bool {
    text.chars().all(|ch| {
        ch.is_ascii_alphanumeric()
            || matches!(
                ch,
                '-' | '.'
                    | '_'
                    | ':'
                    | '~'
                    | '!'
                    | '$'
                    | '&'
                    | '\''
                    | '('
                    | ')'
                    | '*'
                    | '+'
                    | ','
                    | ';'
                    | '='
                    | '%'
            )
    })
}

fn is_hex(byte: u8) -> bool {
    byte.is_ascii_hexdigit()
}

/// The value of one hex digit. Only called for `is_hex` bytes.
fn unhex(byte: u8) -> u8 {
    match byte {
        b'0'..=b'9' => byte - b'0',
        b'a'..=b'f' => byte - b'a' + 10,
        _ => byte - b'A' + 10,
    }
}

/// `shouldEscape`, transcribed from Go's generated table generator.
fn should_escape(byte: u8, mode: Mode) -> bool {
    if byte.is_ascii_alphanumeric() {
        return false;
    }
    if matches!(mode, Mode::Host | Mode::Zone) {
        // Host includes the sub-delims, plus `:` and `[]` because the port and
        // the brackets are part of it, plus `<>` and `"` because they are the
        // only characters left that a host could hold.
        if matches!(
            byte,
            b'!' | b'$'
                | b'&'
                | b'\''
                | b'('
                | b')'
                | b'*'
                | b'+'
                | b','
                | b';'
                | b'='
                | b':'
                | b'['
                | b']'
                | b'<'
                | b'>'
                | b'"'
        ) {
            return false;
        }
    }
    match byte {
        b'-' | b'_' | b'.' | b'~' => false,
        b'$' | b'&' | b'+' | b',' | b'/' | b':' | b';' | b'=' | b'?' | b'@' => match mode {
            Mode::Path => byte == b'?',
            Mode::UserPassword => matches!(byte, b'@' | b'/' | b'?' | b':'),
            Mode::Fragment => false,
            Mode::Host | Mode::Zone => true,
        },
        _ => {
            if mode == Mode::Fragment {
                // RFC 3986 allows the sub-delims in a fragment unescaped.
                if matches!(byte, b'!' | b'(' | b')' | b'*') {
                    return false;
                }
            }
            true
        }
    }
}

/// `unescape`: decode `%XX`, refuse what the mode refuses.
fn unescape(text: &str, mode: Mode) -> Result<String, String> {
    let bytes = text.as_bytes();
    let mut escapes = 0usize;
    let mut i = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'%' => {
                escapes += 1;
                if i + 2 >= bytes.len() || !is_hex(bytes[i + 1]) || !is_hex(bytes[i + 2]) {
                    let end = (i + 3).min(bytes.len());
                    return Err(format!(
                        "invalid URL escape {}",
                        quote_slice(&bytes[i..end])
                    ));
                }
                if mode == Mode::Host && unhex(bytes[i + 1]) < 8 && &bytes[i..i + 3] != b"%25" {
                    // A host may escape non-ASCII only; `%25` is the one
                    // exception, for the zone introducer.
                    return Err(format!(
                        "invalid URL escape {}",
                        quote_slice(&bytes[i..i + 3])
                    ));
                }
                if mode == Mode::Zone {
                    let value = unhex(bytes[i + 1]) << 4 | unhex(bytes[i + 2]);
                    if &bytes[i..i + 3] != b"%25"
                        && value != b' '
                        && should_escape(value, Mode::Host)
                    {
                        return Err(format!(
                            "invalid URL escape {}",
                            quote_slice(&bytes[i..i + 3])
                        ));
                    }
                }
                i += 3;
            }
            b'+' => i += 1,
            byte => {
                if matches!(mode, Mode::Host | Mode::Zone)
                    && byte < 0x80
                    && should_escape(byte, mode)
                {
                    return Err(format!(
                        "invalid character {} in host name",
                        quote_slice(&bytes[i..i + 1])
                    ));
                }
                i += 1;
            }
        }
    }
    if escapes == 0 {
        return Ok(text.to_owned());
    }

    let mut out = Vec::with_capacity(bytes.len() - 2 * escapes);
    let mut i = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'%' => {
                out.push(unhex(bytes[i + 1]) << 4 | unhex(bytes[i + 2]));
                i += 3;
            }
            // A `+` is literal outside the query component, and the query is
            // never unescaped here.
            byte => {
                out.push(byte);
                i += 1;
            }
        }
    }
    Ok(String::from_utf8_lossy(&out).into_owned())
}

/// Go's `%q` for a byte slice, including the `\xNN` spelling of bytes that are
/// not UTF-8.
fn quote_slice(bytes: &[u8]) -> String {
    match std::str::from_utf8(bytes) {
        Ok(text) => text::quote(text),
        Err(_) => {
            let mut out = String::with_capacity(bytes.len() + 2);
            out.push('"');
            for byte in bytes {
                match byte {
                    b'"' | b'\\' => {
                        out.push('\\');
                        out.push(char::from(*byte));
                    }
                    0x20..=0x7e => out.push(char::from(*byte)),
                    _ => out.push_str(&format!("\\x{byte:02x}")),
                }
            }
            out.push('"');
            out
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parsed(raw: &str) -> Url {
        match parse(raw) {
            Ok(url) => url,
            Err(err) => panic!("{raw:?} should parse: {err}"),
        }
    }

    fn refused(raw: &str) -> ParseError {
        match parse(raw) {
            Ok(_) => panic!("{raw:?} should be refused"),
            Err(err) => err,
        }
    }

    #[test]
    fn a_proxy_url_parses_into_the_fields_core_reads() {
        let url = parsed("socks5://user:pass@proxy.example:1080");
        assert_eq!(url.scheme, "socks5");
        assert_eq!(url.host, "proxy.example:1080");
        assert_eq!(
            url.user,
            Some(Userinfo {
                username: "user".to_owned(),
                password: Some("pass".to_owned()),
            })
        );
    }

    #[test]
    fn a_bracketed_literal_keeps_its_brackets_and_loses_its_zone_escape() {
        let url = parsed("http://[fe80::1%25en0]:8080");
        assert_eq!(url.host, "[fe80::1%en0]:8080");
    }

    #[test]
    fn the_fragment_is_quoted_with_the_whole_input_when_it_is_bad() {
        let err = refused("http://h/%zz#f%zz");
        assert_eq!(
            err.to_string(),
            "parse \"http://h/%zz\": invalid URL escape \"%zz\""
        );
        let err = refused("http://h#f%zz");
        assert_eq!(
            err.to_string(),
            "parse \"http://h#f%zz\": invalid URL escape \"%zz\""
        );
    }

    #[test]
    fn a_control_byte_is_refused_before_anything_else() {
        let err = refused("http://h:80\x01");
        assert_eq!(
            err.to_string(),
            "parse \"http://h:80\\x01\": net/url: invalid control character in URL"
        );
    }
}

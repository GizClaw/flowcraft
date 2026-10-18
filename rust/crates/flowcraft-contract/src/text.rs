//! Go's text conventions, for the sentences core writes.
//!
//! The refusals and validation sentences the port reproduces carry `%s` and
//! `%q` verbs. `%s` is an interpolation; `%q` is `strconv.Quote`, with escapes
//! of its own, and this module is that port.

/// `strconv.Quote`.
///
/// The escapes are Go's, exactly: `\"` and `\\`, the C escapes
/// `\a \b \f \n \r \t \v`, `\xNN` for the remaining C0 controls and DEL,
/// `\uNNNN` for a non-printable character below U+10000, and `\U00NNNNNN`
/// above. Printable characters are literal -- `é` and `日本語` come out as
/// themselves.
///
/// "Printable" is Go's `strconv.IsPrint`: the ASCII decision (0x20..=0x7e) is
/// exact here, and above it this refuses the categories Go refuses by the
/// names it refuses them under -- the Unicode spaces, the format characters,
/// the private-use areas and the noncharacters. What the port does not carry
/// is Go's table of *unassigned* code points, which is only as fresh as the
/// Unicode release the toolchain shipped; a code point unassigned there and
/// assigned here prints literally where Go escapes it. The full table-driven
/// port already exists in `flowcraft-utils::decode` (it was written for the
/// decode refusals, with the tables copied from the release the oracle runs
/// on); consolidating the two downwards is a step of its own, and until it
/// happens the sentences reachable from this crate are exact for the inputs
/// core builds them from -- media types, kinds, encodings, roles, formats.
pub fn quote(text: &str) -> String {
    let mut out = String::with_capacity(text.len() + 2);
    out.push('"');
    for ch in text.chars() {
        match ch {
            '"' | '\\' => {
                out.push('\\');
                out.push(ch);
            }
            '\u{7}' => out.push_str("\\a"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{b}' => out.push_str("\\v"),
            _ if is_print(ch) => out.push(ch),
            _ if (ch as u32) < 0x20 || ch == '\u{7f}' => {
                out.push_str(&format!("\\x{:02x}", ch as u32));
            }
            _ if (ch as u32) < 0x10000 => out.push_str(&format!("\\u{:04x}", ch as u32)),
            _ => out.push_str(&format!("\\U{:08x}", ch as u32)),
        }
    }
    out.push('"');
    out
}

/// `strings.EqualFold`: do two strings differ only in case?
///
/// Go compares rune by rune through `unicode.SimpleFold` cycles, so it is not a
/// lowercase comparison: `K` (U+212A KELVIN SIGN) folds with `k`, `ſ` with
/// `s`, `µ` (U+00B5 MICRO SIGN) with `μ` (U+03BC), and final sigma `ς` with
/// `σ`. Length is not compared first either: `ſ` and `s` are one rune each, but
/// `ß` and `SS` are *not* equal (simple folding keeps `ß`; only full folding
/// expands it).
///
/// The port reaches the same cycle without a folding table by going up and back
/// down: the uppercase form and then its lowercase, when both are single
/// characters. Every cycle above is closed by that step, and the characters
/// where it is not a no-op are the ones Unicode's case mappings already model
/// as one-to-one. What it cannot represent is a character whose full lowercase
/// mapping expands (`İ` -> `i` + U+0307): those do not have a simple fold to
/// their base letter in Go either, so the two agree on refusing them.
pub fn equal_fold(left: &str, right: &str) -> bool {
    if left == right {
        return true;
    }
    let mut left = left.chars();
    let mut right = right.chars();
    loop {
        match (left.next(), right.next()) {
            (None, None) => return true,
            (Some(a), Some(b)) if a == b => continue,
            (Some(a), Some(b)) if a.is_ascii() && b.is_ascii() => {
                if !a.eq_ignore_ascii_case(&b) {
                    return false;
                }
            }
            (Some(a), Some(b)) => {
                if fold_key(a) != fold_key(b) {
                    return false;
                }
            }
            _ => return false,
        }
    }
}

/// One character's simple-fold cycle, as a representative: lowercase of the
/// uppercase form, falling back to whichever single-character mapping exists.
fn fold_key(ch: char) -> char {
    if let Some(upper) = single_case(ch.to_uppercase()) {
        return single_case(upper.to_lowercase()).unwrap_or(upper);
    }
    single_case(ch.to_lowercase()).unwrap_or(ch)
}

/// The only character a case mapping produced, if it produced exactly one.
fn single_case(mut mapped: impl Iterator<Item = char>) -> Option<char> {
    let first = mapped.next()?;
    match mapped.next() {
        None => Some(first),
        Some(_) => None,
    }
}

/// `strconv.IsPrint`: letters, marks, numbers, punctuation and symbols are
/// printable, and of the separators only the ASCII space is.
fn is_print(ch: char) -> bool {
    if ch.is_ascii() {
        return ('\u{20}'..='\u{7e}').contains(&ch);
    }
    !ch.is_control() && !is_non_printable_nonascii(ch as u32)
}

/// The non-ASCII code points Go's tables mark unprintable: separators other
/// than ASCII space, format characters, private use, and noncharacters.
fn is_non_printable_nonascii(rune: u32) -> bool {
    const RANGES: [(u32, u32); 26] = [
        // Separators (Zs) other than U+0020.
        (0x00A0, 0x00A0),
        (0x1680, 0x1680),
        (0x2000, 0x200A),
        (0x2028, 0x2029),
        (0x202F, 0x202F),
        (0x205F, 0x205F),
        (0x3000, 0x3000),
        // Format characters (Cf).
        (0x00AD, 0x00AD),
        (0x0600, 0x0605),
        (0x061C, 0x061C),
        (0x06DD, 0x06DD),
        (0x070F, 0x070F),
        (0x180E, 0x180E),
        (0x200B, 0x200F),
        (0x202A, 0x202E),
        (0x2060, 0x2064),
        (0x2066, 0x206F),
        (0xFEFF, 0xFEFF),
        (0xFFF9, 0xFFFB),
        (0x110BD, 0x110BD),
        (0x1D173, 0x1D17A),
        // Private use.
        (0xE000, 0xF8FF),
        (0xF0000, 0xFFFFD),
        (0x100000, 0x10FFFD),
        // Noncharacters.
        (0xFDD0, 0xFDEF),
        (0xFFFE, 0xFFFF),
    ];
    if RANGES
        .iter()
        .any(|&(low, high)| (low..=high).contains(&rune))
    {
        return true;
    }
    // The last two code points of every plane are noncharacters.
    rune % 0x1_0000 >= 0xFFFE
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Go's answers, including the cycles a lowercase comparison misses and the
    /// pairs a full-fold comparison would wrongly accept.
    #[test]
    fn folding_matches_strings_equal_fold() {
        for (left, right, expected) in [
            ("root", "ROOT", true),
            ("Root", "rOOt", true),
            ("", "", true),
            ("", "a", false),
            ("ab", "abc", false),
            // The four non-ASCII simple folds.
            ("\u{212a}", "k", true),
            ("\u{17f}", "s", true),
            ("\u{b5}", "\u{3bc}", true),
            ("\u{3c2}", "\u{3c3}", true),
            // Full folding would expand these; simple folding does not.
            ("\u{df}", "SS", false),
            ("\u{130}", "i", false),
            // Case is per rune, not per byte.
            ("日本", "日本", true),
            ("é", "É", true),
        ] {
            assert_eq!(equal_fold(left, right), expected, "{left:?} vs {right:?}");
        }
    }

    /// The expectations are Go's own answers; the exotic ones are the same
    /// characters `flowcraft-utils`' port of the tables is pinned with, so
    /// the two implementations can be compared before they are merged.
    #[test]
    fn quoting_matches_strconv_quote() {
        assert_eq!(quote("a"), "\"a\"");
        assert_eq!(quote(""), "\"\"");
        assert_eq!(quote("\u{7}"), "\"\\a\"");
        assert_eq!(quote("\u{0}"), "\"\\x00\"");
        assert_eq!(quote("\u{7f}"), "\"\\x7f\"");
        assert_eq!(quote("é"), "\"é\"");
        assert_eq!(quote("\u{a0}"), "\"\\u00a0\"");
        assert_eq!(quote("\u{2028}"), "\"\\u2028\"");
        assert_eq!(quote("\u{3000}"), "\"\\u3000\"");
        assert_eq!(quote("\u{e000}"), "\"\\ue000\"");
        assert_eq!(quote("\u{feff}"), "\"\\ufeff\"");
        assert_eq!(quote("\u{fffe}"), "\"\\ufffe\"");
        assert_eq!(quote("\u{1d173}"), "\"\\U0001d173\"");
        assert_eq!(quote("\u{10ffff}"), "\"\\U0010ffff\"");
        assert_eq!(quote("🙂"), "\"🙂\"");
        assert_eq!(quote("\u{2ff0}"), "\"⿰\"");
        assert_eq!(quote("\"\\"), "\"\\\"\\\\\"");
        assert_eq!(quote("日本"), "\"日本\"");
    }
}

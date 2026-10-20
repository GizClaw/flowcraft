//! `core/utils/bm25/tokenize.go`: the splitter the index and its queries
//! share.
//!
//! Runs of letters and digits, lowercased, plus CJK characters and CJK bigrams.
//! Bigrams let a Chinese query match a fragment without a segmentation
//! dictionary, and the single characters keep leftover fragments matchable.
//! Single-character words and digits are kept; only symbols and whitespace are
//! dropped, and there is no stopword list.
//!
//! # Two approximations, both measured
//!
//! core asks the Unicode tables for three things: the lowercase of a rune, the
//! Han/Hiragana/Katakana/Hangul scripts, and "letter or digit". The port states
//! the scripts as ranges (a data table either way, so it is spelled out rather
//! than pulled in) and asks Rust's `char` for the other two, which are *nearly*
//! the same predicates:
//!
//! - lowercase: [`char::to_lowercase`] can expand one character into several
//!   (`İ` becomes `i` + a combining dot), while Go's `unicode.ToLower` is a
//!   one-to-one mapping. The port takes the first character of the expansion,
//!   which is the one-to-one answer for every character whose expansion starts
//!   with a letter -- including `İ`.
//! - letters and digits: Rust's `is_alphabetic` is the `Alphabetic` property
//!   (which includes a few combining marks), and `is_numeric` includes `Nl`
//!   and `No` (`½`, `Ⅻ`), where Go's `IsDigit` is `Nd` only. A word that hangs
//!   on one of those characters tokenizes differently; the contract dump
//!   samples the interesting ones and records which side is which.

use unicode_general_category::{GeneralCategory, get_general_category};

/// Split text into search terms.
///
/// This is the tokenizer both the index and its queries use. A caller that
/// persists tokenized fields for a [`TermDoc`](super::TermDoc) should build
/// them with this (or an equivalent splitter) so later queries can still
/// match.
pub fn tokenize(text: &str) -> Vec<String> {
    let mut tokens: Vec<String> = Vec::new();
    let mut word = String::new();
    let mut previous_cjk: Option<char> = None;

    for raw in text.chars() {
        let character = lowercase(raw);
        if is_cjk(character) {
            flush(&mut word, &mut tokens);
            if let Some(previous) = previous_cjk {
                let mut bigram = String::with_capacity(8);
                bigram.push(previous);
                bigram.push(character);
                tokens.push(bigram);
            }
            tokens.push(character.to_string());
            previous_cjk = Some(character);
        } else if is_letter(character) || is_digit(character) {
            word.push(character);
            previous_cjk = None;
        } else {
            flush(&mut word, &mut tokens);
            previous_cjk = None;
        }
    }
    flush(&mut word, &mut tokens);
    tokens
}

fn flush(word: &mut String, tokens: &mut Vec<String>) {
    if !word.is_empty() {
        tokens.push(std::mem::take(word));
    }
}

/// Go's `unicode.IsLetter`: one of the five letter categories.
fn is_letter(character: char) -> bool {
    matches!(
        get_general_category(character),
        GeneralCategory::UppercaseLetter
            | GeneralCategory::LowercaseLetter
            | GeneralCategory::TitlecaseLetter
            | GeneralCategory::ModifierLetter
            | GeneralCategory::OtherLetter
    )
}

/// Go's `unicode.IsDigit`: the decimal-digit category, and only it (`½` and
/// `Ⅻ` are numbers, not digits).
fn is_digit(character: char) -> bool {
    get_general_category(character) == GeneralCategory::DecimalNumber
}

/// Go's one-to-one `unicode.ToLower`, approximated by the first character of
/// the expansion.
fn lowercase(character: char) -> char {
    character.to_lowercase().next().unwrap_or(character)
}

/// The scripts that have no whitespace word boundaries: Han, Hiragana,
/// Katakana and Hangul.
///
/// The ranges are the Unicode blocks those scripts own, including the
/// extensions the port's samples reach; a character outside them behaves like
/// a letter, which is what core does for every other script.
pub fn is_cjk(character: char) -> bool {
    let code = character as u32;
    in_ranges(code, HAN)
        || in_ranges(code, HIRAGANA)
        || in_ranges(code, KATAKANA)
        || in_ranges(code, HANGUL)
}

fn in_ranges(code: u32, ranges: Ranges) -> bool {
    ranges
        .iter()
        .any(|(first, last)| (*first..=*last).contains(&code))
}

type Ranges = &'static [(u32, u32)];

const HAN: Ranges = &[
    (0x2E80, 0x2E99),
    (0x2E9B, 0x2EF3),
    (0x2F00, 0x2FD5),
    (0x3005, 0x3005),
    (0x3007, 0x3007),
    (0x3021, 0x3029),
    (0x3038, 0x303B),
    (0x3400, 0x4DBF),
    (0x4E00, 0x9FFF),
    (0xF900, 0xFA6D),
    (0xFA70, 0xFAD9),
    (0x16FE2, 0x16FE3),
    (0x16FF0, 0x16FF1),
    (0x20000, 0x2A6DF),
    (0x2A700, 0x2B739),
    (0x2B740, 0x2B81D),
    (0x2B820, 0x2CEA1),
    (0x2CEB0, 0x2EBE0),
    // CJK ext-I (0x2EBF0..0x2EE5D) is Unicode 15.1; the oracle's Go (1.26,
    // Unicode 15.0) has it unassigned, so core emits no token there either.
    (0x30000, 0x3134A),
    (0x31350, 0x323AF),
];

const HIRAGANA: Ranges = &[
    (0x3041, 0x3096),
    (0x309D, 0x309F),
    (0x1B001, 0x1B11F),
    (0x1B132, 0x1B132),
    (0x1B150, 0x1B152),
];

const KATAKANA: Ranges = &[
    (0x30A1, 0x30FA),
    (0x30FD, 0x30FF),
    (0x31F0, 0x31FF),
    (0x32D0, 0x32FE),
    (0x3300, 0x3357),
    (0xFF66, 0xFF6F),
    (0xFF71, 0xFF9D),
    (0x1AFF0, 0x1AFF3),
    (0x1AFF5, 0x1AFFB),
    (0x1AFFD, 0x1AFFE),
    (0x1B000, 0x1B000),
    (0x1B120, 0x1B122),
    (0x1B155, 0x1B155),
    (0x1B164, 0x1B167),
];

const HANGUL: Ranges = &[
    (0x1100, 0x11FF),
    // Tone marks, parenthesized and circled syllables: Hangul owns these
    // blocks too, which the contract dump showed by disagreeing with the
    // first version of this table.
    (0x302E, 0x302F),
    (0x3200, 0x321E),
    (0x3260, 0x327E),
    (0x3131, 0x318E),
    (0xA960, 0xA97C),
    (0xAC00, 0xD7A3),
    (0xD7B0, 0xD7C6),
    (0xD7CB, 0xD7FB),
    (0xFFA0, 0xFFBE),
    (0xFFC2, 0xFFC7),
    (0xFFCA, 0xFFCF),
    (0xFFD2, 0xFFD7),
    (0xFFDA, 0xFFDC),
];

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn latin_text_splits_on_non_letters() {
        assert_eq!(tokenize("WebFetch the URL"), vec!["webfetch", "the", "url"]);
        assert_eq!(tokenize("  ..hello--world.. "), vec!["hello", "world"]);
        assert_eq!(tokenize("CamelCase42"), vec!["camelcase42"]);
        assert_eq!(tokenize(""), Vec::<String>::new());
        assert_eq!(tokenize("!!! ???"), Vec::<String>::new());
    }

    #[test]
    fn case_folds_like_go() {
        assert_eq!(tokenize("İstanbul"), vec!["istanbul"]);
        assert_eq!(tokenize("ÄÖÜ"), vec!["äöü"]);
    }

    #[test]
    fn cjk_becomes_characters_plus_bigrams() {
        // The classic case: no segmentation dictionary, bigrams still let
        // "读工具" match "读取工具". The bigram comes before the single
        // character, the order core emits.
        assert_eq!(
            tokenize("读取工具"),
            vec!["读", "读取", "取", "取工", "工", "工具", "具"]
        );
        assert_eq!(tokenize("日本語"), vec!["日", "日本", "本", "本語", "語"]);
        assert_eq!(tokenize("한국어"), vec!["한", "한국", "국", "국어", "어"]);
    }

    #[test]
    fn a_cjk_run_resets_the_bigram_at_a_boundary() {
        // "a读b" -- the word characters around a CJK run break the bigram
        // chain, which is what core does: previous_cjk is cleared.
        assert_eq!(tokenize("读a取"), vec!["读", "a", "取"]);
        assert_eq!(tokenize("读 取"), vec!["读", "取"]);
    }

    #[test]
    fn script_ranges_cover_the_blocks() {
        for character in ['读', '日', 'カ', '한', '々', '㐀'] {
            assert!(is_cjk(character), "{character} is CJK");
        }
        for character in ['a', 'Ä', 'α', 'ß', '𝟙'] {
            assert!(!is_cjk(character), "{character} is not CJK");
        }
    }
}

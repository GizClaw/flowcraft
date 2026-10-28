//! Tokenization shared by every text-retrieval primitive.

/// Split text into search terms: runs of letters and digits, lowercased, plus
/// CJK characters and adjacent bigrams.
///
/// Only symbols and whitespace are dropped; no stopwords are removed.
#[must_use]
pub fn tokenize(text: &str) -> Vec<String> {
    let lowered = text.to_lowercase();
    let mut tokens = Vec::new();
    let mut word = String::new();
    let mut previous_cjk: Option<char> = None;
    for character in lowered.chars() {
        if is_cjk(character) {
            if !word.is_empty() {
                tokens.push(std::mem::take(&mut word));
            }
            if let Some(previous) = previous_cjk {
                tokens.push(format!("{previous}{character}"));
            }
            tokens.push(character.to_string());
            previous_cjk = Some(character);
        } else if character.is_alphanumeric() {
            word.push(character);
            previous_cjk = None;
        } else {
            if !word.is_empty() {
                tokens.push(std::mem::take(&mut word));
            }
            previous_cjk = None;
        }
    }
    if !word.is_empty() {
        tokens.push(word);
    }
    tokens
}

fn is_cjk(character: char) -> bool {
    let code = character as u32;
    (0x4E00..=0x9FFF).contains(&code) // Han
        || (0x3040..=0x309F).contains(&code) // Hiragana
        || (0x30A0..=0x30FF).contains(&code) // Katakana
        || (0xAC00..=0xD7AF).contains(&code) // Hangul
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ascii_is_lowercased_and_split_on_symbols() {
        assert_eq!(tokenize("Read_File v2"), vec!["read", "file", "v2"]);
    }

    #[test]
    fn cjk_yields_characters_and_bigrams() {
        assert_eq!(
            tokenize("文件读取"),
            vec!["文", "文件", "件", "件读", "读", "读取", "取"]
        );
    }

    #[test]
    fn single_characters_and_digits_are_kept() {
        assert_eq!(tokenize("a 1"), vec!["a", "1"]);
        assert!(tokenize("  ,.!  ").is_empty());
    }
}

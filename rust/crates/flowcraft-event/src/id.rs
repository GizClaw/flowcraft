//! Envelope identifiers, in `rs/xid`'s shape: 12 bytes -- 4 of unix seconds, 3
//! of machine, 2 of pid, 3 of counter -- written as 20 base-32 characters.
//!
//! The port does not depend on the Go library; what it keeps is what a consumer
//! can observe: the length, the alphabet, and the time ordering (the first
//! characters are the seconds, so a list of ids sorts by creation time). The
//! machine and pid bytes are only there to keep two processes from minting the
//! same id, which is why a hash of the hostname and the process id are enough.
//!
//! core mints these inside `NewEnvelope` / `Publish`; nothing in the contract
//! depends on the exact bytes.

use std::hash::{Hash, Hasher};
use std::sync::atomic::{AtomicU32, Ordering};

/// The base-32 alphabet `rs/xid` encodes with (hex extended to 32 symbols).
const ALPHABET: &[u8; 32] = b"0123456789abcdefghijklmnopqrstuv";

static COUNTER: AtomicU32 = AtomicU32::new(0);

/// A fresh identifier, 20 characters long and time-ordered.
pub fn next() -> String {
    let secs = match std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH) {
        Ok(elapsed) => elapsed.as_secs() as u32,
        Err(_) => 0,
    };
    let machine = machine_id();
    let pid = std::process::id() as u16;
    let counter = COUNTER.fetch_add(1, Ordering::Relaxed);

    let mut bytes = [0u8; 12];
    bytes[0..4].copy_from_slice(&secs.to_be_bytes());
    bytes[4..7].copy_from_slice(&machine);
    bytes[7..9].copy_from_slice(&pid.to_be_bytes());
    // The counter starts at a random-ish seed so two processes that boot in the
    // same second still differ; three bytes as `xid` keeps.
    let seed = machine[0] as u32 | (machine[1] as u32) << 8 | (machine[2] as u32) << 16;
    let value = (counter.wrapping_add(seed)) & 0x00FF_FFFF;
    bytes[9..12].copy_from_slice(&value.to_be_bytes()[1..4]);
    encode(bytes)
}

/// The machine component: three bytes of a stable hash of the hostname, with a
/// fixed fallback when the hostname cannot be read.
fn machine_id() -> [u8; 3] {
    let hostname = std::env::var("HOSTNAME")
        .unwrap_or_else(|_| "flowcraft".to_owned())
        .trim()
        .to_owned();
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    hostname.hash(&mut hasher);
    let hash = hasher.finish().to_be_bytes();
    [hash[0], hash[1], hash[2]]
}

/// Twelve bytes as twenty characters: five bits at a time from the top, which
/// is what `xid`'s hand-unrolled encoder does.
fn encode(bytes: [u8; 12]) -> String {
    let mut out = String::with_capacity(20);
    let mut accumulator: u32 = 0;
    let mut bits: u32 = 0;
    for byte in bytes {
        accumulator = (accumulator << 8) | u32::from(byte);
        bits += 8;
        while bits >= 5 {
            bits -= 5;
            out.push(ALPHABET[((accumulator >> bits) & 0x1F) as usize] as char);
        }
    }
    if bits > 0 {
        // 96 bits is 19 groups of five plus one bit, padded on the right.
        out.push(ALPHABET[((accumulator << (5 - bits)) & 0x1F) as usize] as char);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn identifiers_have_xids_shape_and_order() {
        let first = next();
        let second = next();
        assert_eq!(first.len(), 20);
        assert!(first.bytes().all(|byte| ALPHABET.contains(&byte)));
        assert_ne!(first, second);
        // The first four bytes are the seconds, so ids minted in order compare
        // in order.
        assert!(second > first);
    }

    #[test]
    fn the_encoder_is_msb_first_five_bit_groups() {
        assert_eq!(encode([0u8; 12]), "00000000000000000000");
        // 95 bits fill nineteen groups; the last byte's low nibble is the
        // twentieth, padded on the right -- which is what xid's unrolled
        // encoder does with `(id[11] << 4) & 0x1F`.
        assert_eq!(encode([0xff; 12]), "vvvvvvvvvvvvvvvvvvvg");
        assert_eq!(
            encode([0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1]),
            "0000000000000000000g"
        );
    }
}

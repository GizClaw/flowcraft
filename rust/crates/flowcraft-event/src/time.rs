//! The timestamp spelling `encoding/json` gives a `time.Time`, and nothing
//! more: this is not a calendar library, it is the envelope's `time` field.
//!
//! Go writes RFC 3339 with nanoseconds, trailing zeros trimmed, and no fraction
//! at all when the nanoseconds are zero (`time.RFC3339Nano`, the format
//! `time.Time.MarshalJSON` uses). The port writes the same grammar, and always
//! in UTC: core writes whatever zone `time.Now()` carried, which is the same
//! instant spelled differently. Consumers parse both; what they may rely on is
//! the instant, and that is what this type stores.
//!
//! Parsing lands with the bus, which is the first thing that reads an envelope
//! back off the wire.

/// An instant: seconds since the Unix epoch plus the sub-second part.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct Timestamp {
    secs: i64,
    nanos: u32,
}

impl Timestamp {
    /// Go's zero `time.Time`, 0001-01-01T00:00:00Z -- what an envelope carries
    /// until a producer fills it in.
    pub const ZERO: Timestamp = Timestamp {
        secs: -62_135_596_800,
        nanos: 0,
    };

    /// An instant from seconds since the epoch and a sub-second part, clamped
    /// to the nanosecond range a timestamp can hold.
    pub fn from_unix(secs: i64, nanos: u32) -> Timestamp {
        Timestamp {
            secs,
            nanos: nanos.min(999_999_999),
        }
    }

    /// The current instant.
    pub fn now() -> Timestamp {
        match std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH) {
            Ok(elapsed) => Timestamp::from_unix(elapsed.as_secs() as i64, elapsed.subsec_nanos()),
            // Before 1970 only if the clock is wrong; the arithmetic below is
            // signed, so the answer is still an instant.
            Err(err) => {
                let elapsed = err.duration();
                Timestamp::from_unix(
                    -(elapsed.as_secs() as i64),
                    1_000_000_000 - elapsed.subsec_nanos(),
                )
            }
        }
    }

    /// Whether this is [`Timestamp::ZERO`].
    pub fn is_zero(self) -> bool {
        self == Timestamp::ZERO
    }

    /// Seconds since the epoch.
    pub fn secs(self) -> i64 {
        self.secs
    }

    /// The sub-second part, in nanoseconds.
    pub fn nanos(self) -> u32 {
        self.nanos
    }

    /// `time.Time.MarshalJSON`: `2006-01-02T15:04:05.999999999Z`.
    pub fn to_rfc3339(self) -> String {
        let days = self.secs.div_euclid(86_400);
        let rest = self.secs.rem_euclid(86_400);
        let (year, month, day) = civil_from_days(days);
        let (hour, minute, second) = (rest / 3600, rest % 3600 / 60, rest % 60);
        let mut out = format!("{year:04}-{month:02}-{day:02}T{hour:02}:{minute:02}:{second:02}");
        if self.nanos != 0 {
            let fraction = format!("{:09}", self.nanos);
            out.push('.');
            out.push_str(fraction.trim_end_matches('0'));
        }
        out.push('Z');
        out
    }
}

impl From<Timestamp> for String {
    fn from(timestamp: Timestamp) -> String {
        timestamp.to_rfc3339()
    }
}

/// Days since 1970-01-01 to a proleptic Gregorian date (Howard Hinnant's
/// `civil_from_days`, which is what Go's `time.Date` does for the same input).
fn civil_from_days(days: i64) -> (i64, u32, u32) {
    let z = days + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let day_of_era = (z - era * 146_097) as u64;
    let year_of_era =
        (day_of_era - day_of_era / 1460 + day_of_era / 36_524 - day_of_era / 146_096) / 365;
    let year = year_of_era as i64 + era * 400;
    let day_of_year = day_of_era - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    let mp = (5 * day_of_year + 2) / 153;
    let day = (day_of_year - (153 * mp + 2) / 5 + 1) as u32;
    let month = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (if month <= 2 { year + 1 } else { year }, month, day)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn formats_the_way_encoding_json_formats_a_time() {
        // Copied from `json.Marshal(time.Unix(...).UTC())`.
        assert_eq!(
            Timestamp::from_unix(1_760_572_800, 0).to_rfc3339(),
            "2025-10-16T00:00:00Z"
        );
        assert_eq!(
            Timestamp::from_unix(1_760_572_800, 123_456_789).to_rfc3339(),
            "2025-10-16T00:00:00.123456789Z"
        );
        // Trailing zeros go, in threes or one at a time.
        assert_eq!(
            Timestamp::from_unix(1_760_572_800, 123_000_000).to_rfc3339(),
            "2025-10-16T00:00:00.123Z"
        );
        assert_eq!(
            Timestamp::from_unix(1_760_572_800, 500_000_000).to_rfc3339(),
            "2025-10-16T00:00:00.5Z"
        );
        assert_eq!(
            Timestamp::from_unix(1_760_572_800, 1).to_rfc3339(),
            "2025-10-16T00:00:00.000000001Z"
        );
        assert_eq!(Timestamp::ZERO.to_rfc3339(), "0001-01-01T00:00:00Z");
        // Before the epoch, and a leap day, both from the same civil walk.
        assert_eq!(
            Timestamp::from_unix(-1, 0).to_rfc3339(),
            "1969-12-31T23:59:59Z"
        );
        assert_eq!(
            Timestamp::from_unix(1_709_164_800, 0).to_rfc3339(),
            "2024-02-29T00:00:00Z"
        );
    }

    #[test]
    fn now_is_after_the_epoch_and_not_zero() {
        let now = Timestamp::now();
        assert!(now.secs() > 1_700_000_000);
        assert!(now.nanos() < 1_000_000_000);
        assert!(!now.is_zero());
    }
}

//! Minimal RFC 3339 rendering, UTC, millisecond precision.
//!
//! A timestamp is the only calendar arithmetic this crate needs, so a full date
//! library would be a poor trade. The civil-date conversion is Howard
//! Hinnant's `civil_from_days`, which is exact for the whole `i64` range and
//! has no leap-second or leap-year edge cases beyond the Gregorian rules.

use std::time::{SystemTime, UNIX_EPOCH};

/// `2026-10-23T10:20:31.123Z`, or the epoch if the clock is before it.
pub(crate) fn rfc3339_millis(at: SystemTime) -> String {
    let (seconds, millis) = match at.duration_since(UNIX_EPOCH) {
        Ok(elapsed) => (elapsed.as_secs() as i64, elapsed.subsec_millis()),
        Err(error) => {
            let elapsed = error.duration();
            let seconds = -(elapsed.as_secs() as i64);
            let millis = elapsed.subsec_millis();
            if millis == 0 {
                (seconds, 0)
            } else {
                (seconds - 1, 1000 - millis)
            }
        }
    };
    let days = seconds.div_euclid(86_400);
    let of_day = seconds.rem_euclid(86_400);
    let (year, month, day) = civil_from_days(days);
    let hour = of_day / 3_600;
    let minute = (of_day % 3_600) / 60;
    let second = of_day % 60;
    format!("{year:04}-{month:02}-{day:02}T{hour:02}:{minute:02}:{second:02}.{millis:03}Z")
}

/// Days since 1970-01-01 to a Gregorian calendar date.
fn civil_from_days(days: i64) -> (i64, u32, u32) {
    // Shift the epoch to 0000-03-01 so leap days land at the end of the cycle.
    let shifted = days + 719_468;
    let era = if shifted >= 0 {
        shifted
    } else {
        shifted - 146_096
    } / 146_097;
    let day_of_era = shifted - era * 146_097; // [0, 146096]
    let year_of_era =
        (day_of_era - day_of_era / 1_460 + day_of_era / 36_524 - day_of_era / 146_096) / 365;
    let year = year_of_era + era * 400;
    let day_of_year = day_of_era - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    let month_position = (5 * day_of_year + 2) / 153; // [0, 11]
    let day = (day_of_year - (153 * month_position + 2) / 5 + 1) as u32;
    let month = if month_position < 10 {
        month_position + 3
    } else {
        month_position - 9
    } as u32;
    let year = if month <= 2 { year + 1 } else { year };
    (year, month, day)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    fn at(seconds: u64, millis: u32) -> SystemTime {
        UNIX_EPOCH + Duration::new(seconds, millis * 1_000_000)
    }

    #[test]
    fn epoch_is_zero() {
        assert_eq!(rfc3339_millis(UNIX_EPOCH), "1970-01-01T00:00:00.000Z");
    }

    #[test]
    fn leap_day_and_end_of_year() {
        // 2024-02-29T12:34:56.789Z
        assert_eq!(
            rfc3339_millis(at(1_709_210_096, 789)),
            "2024-02-29T12:34:56.789Z"
        );
        // 2026-12-31T23:59:59.999Z
        assert_eq!(
            rfc3339_millis(at(1_798_761_599, 999)),
            "2026-12-31T23:59:59.999Z"
        );
    }

    #[test]
    fn epochs_before_1970_do_not_panic() {
        let before = UNIX_EPOCH.checked_sub(Duration::from_secs(1));
        assert_eq!(
            before.map(rfc3339_millis),
            Some("1969-12-31T23:59:59.000Z".to_owned())
        );
    }
}

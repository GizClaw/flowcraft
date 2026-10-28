//! Tokens, cost, and how two reports of them add up.
//!
//! Ported from `core/inference/usage/`. The folding rule is the part worth copying
//! exactly: **an unreported counter stays unreported**. `Some(3) + None` is not
//! `Some(3)` in the sense of "the caller reported three"; it means one side said
//! three and the other said nothing, and a host that budgets on these numbers has to
//! be able to tell a real zero from a silence.

use serde::{Deserialize, Serialize};

/// Token counts for one call, in the shape a provider reports them.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Usage {
    /// Prompt tokens, as the provider counted them.
    #[serde(default)]
    pub input_tokens: u64,
    /// Completion tokens.
    #[serde(default)]
    pub output_tokens: u64,
    /// Whatever total the provider reported. Not recomputed: a provider that bills a
    /// different total than the sum of its parts is telling the truth about the bill.
    #[serde(default)]
    pub total_tokens: u64,
    /// Prompt tokens served from the provider's cache.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cache_read_tokens: Option<u64>,
    /// Reasoning tokens, when the provider separates them from the completion.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reasoning_tokens: Option<u64>,
}

impl Usage {
    /// Sum two reports.
    ///
    /// `None` stays `None`: "not reported" must not become a reported zero, or a
    /// budget calculation silently charges a host for a number nobody measured.
    #[must_use]
    pub fn add(&self, other: &Self) -> Self {
        Self {
            input_tokens: self.input_tokens.saturating_add(other.input_tokens),
            output_tokens: self.output_tokens.saturating_add(other.output_tokens),
            total_tokens: self.total_tokens.saturating_add(other.total_tokens),
            cache_read_tokens: add_optional(self.cache_read_tokens, other.cache_read_tokens),
            reasoning_tokens: add_optional(self.reasoning_tokens, other.reasoning_tokens),
        }
    }

    /// Whether the provider reported nothing at all.
    #[must_use]
    pub fn is_unreported(&self) -> bool {
        *self == Self::default()
    }
}

fn add_optional(left: Option<u64>, right: Option<u64>) -> Option<u64> {
    match (left, right) {
        (None, None) => None,
        (left, right) => Some(left.unwrap_or(0).saturating_add(right.unwrap_or(0))),
    }
}

/// A decimal money amount, `units * 10^-scale` in `currency`.
///
/// Deliberately not a float: cost accumulates across many calls, and the whole point
/// of carrying `units`/`scale` is that a host can add them without inventing
/// precision.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Money {
    /// ISO 4217 code, or whatever the deployment's pricing table uses.
    pub currency: String,
    /// The mantissa.
    pub units: i64,
    /// How many decimal places `units` has.
    pub scale: u8,
}

impl Money {
    /// Sum two amounts that share a currency and a scale.
    ///
    /// `None` for a mismatch: converting between currencies or scales is a policy
    /// decision (an exchange rate, a rounding rule) this type does not make. A host
    /// enforcing a single budget normalizes before it adds.
    #[must_use]
    pub fn add(&self, other: &Self) -> Option<Self> {
        if self.currency != other.currency || self.scale != other.scale {
            return None;
        }
        Some(Self {
            currency: self.currency.clone(),
            units: self.units.checked_add(other.units)?,
            scale: self.scale,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn adding_reports_sums_the_counters() {
        let left = Usage {
            input_tokens: 10,
            output_tokens: 4,
            total_tokens: 14,
            cache_read_tokens: Some(2),
            reasoning_tokens: None,
        };
        let right = Usage {
            input_tokens: 5,
            output_tokens: 1,
            total_tokens: 6,
            cache_read_tokens: Some(3),
            reasoning_tokens: Some(7),
        };
        assert_eq!(
            left.add(&right),
            Usage {
                input_tokens: 15,
                output_tokens: 5,
                total_tokens: 20,
                cache_read_tokens: Some(5),
                // One side reported, so the sum is known.
                reasoning_tokens: Some(7),
            }
        );
    }

    /// The rule that makes these numbers usable for budgeting: silence stays
    /// silence, and a real zero stays a real zero.
    #[test]
    fn an_unreported_counter_does_not_become_zero() {
        let silent = Usage::default();
        assert!(silent.is_unreported());
        let reported = Usage {
            cache_read_tokens: Some(0),
            ..Usage::default()
        };
        assert_eq!(
            silent.add(&reported).cache_read_tokens,
            Some(0),
            "a reported zero is a measurement"
        );
        assert_eq!(
            reported.add(&silent).reasoning_tokens,
            None,
            "and a silence is not"
        );
    }

    #[test]
    fn money_adds_only_where_it_can() {
        let usd = Money {
            currency: "USD".to_owned(),
            units: 1_250,
            scale: 6,
        };
        assert_eq!(
            usd.add(&usd),
            Some(Money {
                currency: "USD".to_owned(),
                units: 2_500,
                scale: 6,
            })
        );
        let eur = Money {
            currency: "EUR".to_owned(),
            ..usd.clone()
        };
        let coarse = Money {
            scale: 2,
            ..usd.clone()
        };
        assert_eq!(usd.add(&eur), None, "no exchange rate is invented here");
        assert_eq!(usd.add(&coarse), None, "nor a rounding rule");
    }
}

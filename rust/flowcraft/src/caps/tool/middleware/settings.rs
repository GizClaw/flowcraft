//! The declarative middleware chain (`[tool.middleware]` in a document).

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::time::Duration;

use crate::error::{Error, Result};
use serde::Deserialize;

use super::{
    DEFAULT_PART_BUDGET, ResultLimitOptions, TruncateOptions, concurrency, recover, result_limit,
    result_part_limit, telemetry, timeout, truncate,
};
use crate::caps::tool::executor::Middleware;

/// The middleware subtree of a tool assembly.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Settings {
    /// Recovery net. On unless explicitly disabled.
    #[serde(default)]
    pub recover: Option<RecoverSettings>,
    /// Per-call span and failure log.
    #[serde(default)]
    pub telemetry: Option<TelemetrySettings>,
    /// Text and non-text result budgets.
    #[serde(default)]
    pub result_limit: Option<ResultLimitSettings>,
    /// Recoverable truncation with a spill file.
    #[serde(default)]
    pub truncate: Option<TruncateSettings>,
    /// Per-call deadline.
    #[serde(default)]
    pub timeout: Option<TimeoutSettings>,
    /// In-flight cap.
    #[serde(default)]
    pub concurrency: Option<ConcurrencySettings>,
    /// Non-text budget when `result_limit` is absent.
    #[serde(default)]
    pub result_part_budget_bytes: Option<usize>,
}

/// Recovery settings.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecoverSettings {
    /// `false` installs no net.
    #[serde(default)]
    pub enabled: Option<bool>,
}

/// Telemetry settings.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TelemetrySettings {
    /// Turn the span on.
    #[serde(default)]
    pub enabled: bool,
}

/// Result-limit settings.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResultLimitSettings {
    /// Rune budget for text.
    pub max: usize,
    /// Marker replacing the default.
    #[serde(default)]
    pub marker: Option<String>,
    /// Byte budget for non-text parts; absent keeps the default, zero lifts it.
    #[serde(default)]
    pub part_budget_bytes: Option<usize>,
}

/// Truncate settings.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TruncateSettings {
    /// Turn truncation on.
    #[serde(default)]
    pub enabled: bool,
    /// In-context rune cap.
    #[serde(default)]
    pub max_chars: usize,
    /// Spill directory.
    #[serde(default)]
    pub dir: PathBuf,
    /// Anchors the pointer; empty keeps it absolute.
    #[serde(default)]
    pub work_dir: Option<PathBuf>,
}

/// Timeout settings.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TimeoutSettings {
    /// A duration such as `30s` or `2m`. Empty disables the middleware.
    #[serde(default)]
    pub default: String,
}

/// Concurrency settings.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConcurrencySettings {
    /// In-flight cap; must be positive.
    #[serde(default)]
    pub limit: usize,
}

/// Build the chain declared by `settings`, outermost first.
pub fn from_settings(settings: Settings) -> Result<Vec<Middleware>> {
    let mut chain = Vec::new();

    if settings
        .recover
        .as_ref()
        .and_then(|recover| recover.enabled)
        .unwrap_or(true)
    {
        chain.push(recover());
    }
    if settings
        .telemetry
        .as_ref()
        .is_some_and(|telemetry| telemetry.enabled)
    {
        chain.push(telemetry());
    }

    if let Some(limit) = &settings.result_limit {
        if limit.max == 0 {
            return Err(Error::validation(
                "tool middleware: result_limit.max must be positive",
            ));
        }
        chain.push(result_limit(
            limit.max,
            ResultLimitOptions {
                marker: limit.marker.clone().unwrap_or_default(),
                part_budget: limit.part_budget_bytes.unwrap_or(DEFAULT_PART_BUDGET),
            },
        )?);
    } else if let Some(budget) = settings.result_part_budget_bytes
        && budget > 0
    {
        chain.push(result_part_limit(budget)?);
    }

    if let Some(truncate_settings) = &settings.truncate
        && truncate_settings.enabled
    {
        if truncate_settings.max_chars == 0 {
            return Err(Error::validation(
                "tool middleware: truncate.max_chars must be positive",
            ));
        }
        if truncate_settings.dir.as_os_str().is_empty() {
            return Err(Error::validation(
                "tool middleware: truncate.dir is required",
            ));
        }
        chain.push(truncate(TruncateOptions {
            max_chars: truncate_settings.max_chars,
            dir: truncate_settings.dir.clone(),
            work_dir: truncate_settings.work_dir.clone(),
        })?);
    }

    if let Some(timeout_settings) = &settings.timeout
        && !timeout_settings.default.is_empty()
    {
        let default = parse_duration(&timeout_settings.default).map_err(|error| {
            Error::new(
                error.kind(),
                format!("tool middleware: timeout.default: {error}"),
            )
        })?;
        chain.push(timeout(default, BTreeMap::new()));
    }

    if let Some(concurrency_settings) = &settings.concurrency {
        chain.push(concurrency(concurrency_settings.limit).map_err(|error| {
            Error::new(
                error.kind(),
                format!("tool middleware: concurrency.limit: {error}"),
            )
        })?);
    }

    Ok(chain)
}

/// Parse a Go-style duration: `300ms`, `2m`, `1h30m`.
pub fn parse_duration(value: &str) -> Result<Duration> {
    let value = value.trim();
    if value.is_empty() {
        return Err(Error::validation("duration is empty"));
    }
    let mut total = 0.0_f64;
    let mut rest = value;
    while !rest.is_empty() {
        let number_len = rest
            .find(|character: char| !(character.is_ascii_digit() || character == '.'))
            .unwrap_or(rest.len());
        if number_len == 0 {
            return Err(Error::validation(format!(
                "duration {value:?}: expected a number"
            )));
        }
        let number: f64 = rest[..number_len]
            .parse()
            .map_err(|_| Error::validation(format!("duration {value:?}: invalid number")))?;
        rest = &rest[number_len..];
        let unit_len = rest
            .find(|character: char| character.is_ascii_digit() || character == '.')
            .unwrap_or(rest.len());
        let unit = &rest[..unit_len];
        rest = &rest[unit_len..];
        let seconds = match unit {
            "ns" => 1e-9,
            "us" | "µs" => 1e-6,
            "ms" => 1e-3,
            "s" => 1.0,
            "m" => 60.0,
            "h" => 3600.0,
            _ => {
                return Err(Error::validation(format!(
                    "duration {value:?}: unknown unit {unit:?}"
                )));
            }
        };
        total += number * seconds;
    }
    if !total.is_finite() || total < 0.0 {
        return Err(Error::validation(format!("duration {value:?} is invalid")));
    }
    Ok(Duration::from_secs_f64(total))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn durations_parse_in_go_syntax() {
        assert_eq!(parse_duration("500ms").unwrap(), Duration::from_millis(500));
        assert_eq!(parse_duration("2m").unwrap(), Duration::from_secs(120));
        assert_eq!(parse_duration("1h30m").unwrap(), Duration::from_secs(5400));
        assert!(parse_duration("").is_err());
        assert!(parse_duration("10").is_err());
        assert!(parse_duration("10sx").is_err());
    }

    #[test]
    fn the_default_chain_is_recover_only() {
        let chain = from_settings(Settings::default()).unwrap();
        assert_eq!(chain.len(), 1);
    }

    #[test]
    fn an_invalid_limit_is_refused() {
        let settings = Settings {
            result_limit: Some(ResultLimitSettings {
                max: 0,
                ..ResultLimitSettings::default()
            }),
            ..Settings::default()
        };
        assert!(from_settings(settings).is_err());

        let settings = Settings {
            concurrency: Some(ConcurrencySettings { limit: 0 }),
            ..Settings::default()
        };
        assert!(from_settings(settings).is_err());
    }

    #[test]
    fn a_full_chain_builds_in_order() {
        let settings: Settings = serde_json::from_value(serde_json::json!({
            "telemetry": {"enabled": true},
            "result_limit": {"max": 1024},
            "timeout": {"default": "30s"},
            "concurrency": {"limit": 4}
        }))
        .unwrap();
        let chain = from_settings(settings).unwrap();
        assert_eq!(chain.len(), 5);
    }
}

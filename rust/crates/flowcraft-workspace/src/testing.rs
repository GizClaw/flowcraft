//! What the crate's own tests use to say what they expected.
//!
//! The workspace lints deny `unwrap` and warn on `expect`, tests included, so a
//! failed call is reported as the decision the test was making rather than as
//! whatever the standard library prints.

use crate::{Error, Result};

/// The value of a call the test expects to succeed.
pub fn ok<T>(result: Result<T>) -> T {
    match result {
        Ok(value) => value,
        Err(err) => panic!("expected success, got a refusal: {err}"),
    }
}

/// The refusal of a call the test expects to fail.
pub fn refusal<T: std::fmt::Debug>(result: Result<T>) -> Error {
    match result {
        Ok(value) => panic!("expected a refusal, got {value:?}"),
        Err(err) => err,
    }
}

/// The sentence of a call the test expects to fail.
pub fn refused<T: std::fmt::Debug>(result: Result<T>) -> String {
    refusal(result).to_string()
}

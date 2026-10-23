//! Shared gRPC plumbing.
//!
//! Every capability endpoint speaks the same dialect: a bearer grant in
//! metadata, an endpoint/tenant/capability check, a scope check, and a mapping
//! from the platform's error taxonomy to a gRPC status. Keeping that here means
//! each capability crate only writes its own message handling.

use bytes::Bytes;
use flowcraft_error::{Detail, Error, ErrorKind, Retry};
use flowcraft_proto::error as wire;
use prost::Message as _;
use tonic::{Code, Request, Status};

use flowcraft_kernel::catalog::Endpoint;
use flowcraft_kernel::grant::Grant;
use flowcraft_kernel::handles::Grants;

/// Map the platform's error taxonomy onto one gRPC status.
///
/// The status carries the category twice on purpose: as a [`Code`] for generic
/// middleware, load balancers, and retry libraries that only read statuses, and
/// as a `flowcraft.error.v1.Error` payload for a peer that needs the category
/// itself. The code alone is lossy — `BudgetExceeded` and `RateLimit` both map
/// to `ResourceExhausted`, `PolicyDenied` and `Forbidden` both map to
/// `PermissionDenied` — so a peer reading only the code would treat a permanent
/// budget failure as retryable and a policy refusal as a permission problem.
///
/// Every error is encoded, including one that carries no detail: the category is
/// the whole reason the payload exists, and a handful of protobuf bytes on a
/// failed call is not worth losing it over.
pub fn status(error: Error) -> Status {
    let message = error.to_string();
    Status::with_details(code_of(error.kind()), message, encode(&error))
}

/// Map a gRPC status back to the platform taxonomy, for client call sites.
///
/// The structured payload wins over the code when present; a peer that sends
/// none (an older build, or a non-FlowCraft server) is read through the code
/// exactly as before, so the change is additive.
#[must_use]
pub fn from_status(status: Status) -> Error {
    let fallback = kind_of(status.code(), status.message());
    let Some(payload) = decode(status.details()) else {
        return Error::new(fallback, status.message().to_owned());
    };
    let kind = kind_from_wire(payload.kind).unwrap_or(fallback);
    // The status message is always present; prefer the payload's copy only when
    // it says something.
    let message = if payload.message.is_empty() {
        status.message().to_owned()
    } else {
        payload.message
    };
    let mut error = Error::new(kind, message);
    for detail in payload.details {
        error = error.with_detail(detail_from_wire(detail));
    }
    if let Some(retry) = payload.retry.and_then(retry_from_wire) {
        error = error.with_retry(retry);
    }
    if !payload.request_id.is_empty() {
        error = error.with_request_id(payload.request_id);
    }
    error
}

/// The coarse status code for a category.
fn code_of(kind: ErrorKind) -> Code {
    match kind {
        ErrorKind::Validation => Code::InvalidArgument,
        ErrorKind::NotFound => Code::NotFound,
        ErrorKind::Unauthorized => Code::Unauthenticated,
        ErrorKind::Forbidden => Code::PermissionDenied,
        ErrorKind::Conflict => Code::AlreadyExists,
        ErrorKind::RateLimit => Code::ResourceExhausted,
        ErrorKind::Timeout => Code::DeadlineExceeded,
        ErrorKind::Interrupted => Code::Cancelled,
        ErrorKind::Aborted => Code::Aborted,
        ErrorKind::Closed => Code::FailedPrecondition,
        ErrorKind::NotAvailable => Code::Unavailable,
        ErrorKind::BudgetExceeded => Code::ResourceExhausted,
        ErrorKind::PolicyDenied => Code::PermissionDenied,
        _ => Code::Internal,
    }
}

/// The approximate category for a status that carries no structured payload.
fn kind_of(code: Code, message: &str) -> ErrorKind {
    match code {
        Code::InvalidArgument => ErrorKind::Validation,
        Code::NotFound => ErrorKind::NotFound,
        Code::Unauthenticated => ErrorKind::Unauthorized,
        Code::PermissionDenied => ErrorKind::Forbidden,
        Code::AlreadyExists => ErrorKind::Conflict,
        Code::ResourceExhausted => ErrorKind::RateLimit,
        Code::DeadlineExceeded => ErrorKind::Timeout,
        Code::Cancelled => ErrorKind::Interrupted,
        Code::Aborted => ErrorKind::Aborted,
        Code::FailedPrecondition => ErrorKind::Closed,
        Code::Unavailable | Code::Unimplemented => ErrorKind::NotAvailable,
        // Tonic reports client-side IO/TLS failures as `Unknown("transport
        // error")`. That is a reachability problem, not an internal bug, so it
        // keeps the retryable classification a caller expects.
        Code::Unknown if message.contains("transport error") => ErrorKind::NotAvailable,
        _ => ErrorKind::Internal,
    }
}

fn encode(error: &Error) -> Bytes {
    let payload = wire::Error {
        kind: kind_to_wire(error.kind()) as i32,
        message: error.message().to_owned(),
        details: error.details().iter().map(detail_to_wire).collect(),
        retry: error.retry().and_then(retry_to_wire),
        request_id: error.request_id().unwrap_or_default().to_owned(),
    };
    Bytes::from(payload.encode_to_vec())
}

fn decode(bytes: &[u8]) -> Option<wire::Error> {
    if bytes.is_empty() {
        return None;
    }
    wire::Error::decode(bytes).ok()
}

fn detail_to_wire(detail: &Detail) -> wire::Detail {
    wire::Detail {
        domain: detail.domain().to_owned(),
        code: detail.code().to_owned(),
        message: detail.message().to_owned(),
        fields: detail.fields().clone().into_iter().collect(),
    }
}

fn detail_from_wire(detail: wire::Detail) -> Detail {
    Detail::new(detail.domain, detail.code)
        .with_message(detail.message)
        .with_fields(detail.fields)
}

/// Encode a retry block, or nothing when it reports nothing.
fn retry_to_wire(retry: Retry) -> Option<wire::RetryInfo> {
    if retry.is_empty() {
        return None;
    }
    let millis = retry.backoff().map_or(0, |after| {
        u64::try_from(after.as_millis()).unwrap_or(u64::MAX)
    });
    Some(wire::RetryInfo {
        retry_after_millis: millis,
        wire_attempts: retry.wire_attempts(),
    })
}

fn retry_from_wire(retry: wire::RetryInfo) -> Option<Retry> {
    let mut decoded = Retry::default();
    if retry.retry_after_millis > 0 {
        decoded = decoded.with_after(std::time::Duration::from_millis(retry.retry_after_millis));
    }
    if retry.wire_attempts > 0 {
        decoded = decoded.with_attempts(retry.wire_attempts);
    }
    if decoded.is_empty() {
        return None;
    }
    Some(decoded)
}

/// The wire value for a category.
///
/// `ErrorKind` is `#[non_exhaustive]`, so a category added in a newer
/// `flowcraft-error` lands in the fallback arm. Sending `UNSPECIFIED` is the
/// honest answer: the peer falls back to the status code instead of trusting a
/// guess. `every_kind_survives_the_round_trip` covers `ErrorKind::ALL`, so a
/// category cannot quietly start doing that.
fn kind_to_wire(kind: ErrorKind) -> wire::Kind {
    use wire::Kind;
    match kind {
        ErrorKind::Validation => Kind::Validation,
        ErrorKind::NotFound => Kind::NotFound,
        ErrorKind::Unauthorized => Kind::Unauthorized,
        ErrorKind::Forbidden => Kind::Forbidden,
        ErrorKind::Conflict => Kind::Conflict,
        ErrorKind::RateLimit => Kind::RateLimit,
        ErrorKind::Timeout => Kind::Timeout,
        ErrorKind::Interrupted => Kind::Interrupted,
        ErrorKind::Aborted => Kind::Aborted,
        ErrorKind::Closed => Kind::Closed,
        ErrorKind::NotAvailable => Kind::NotAvailable,
        ErrorKind::Internal => Kind::Internal,
        ErrorKind::BudgetExceeded => Kind::BudgetExceeded,
        ErrorKind::PolicyDenied => Kind::PolicyDenied,
        _ => Kind::Unspecified,
    }
}

fn kind_from_wire(raw: i32) -> Option<ErrorKind> {
    use wire::Kind;
    match Kind::try_from(raw).ok()? {
        Kind::Validation => Some(ErrorKind::Validation),
        Kind::NotFound => Some(ErrorKind::NotFound),
        Kind::Unauthorized => Some(ErrorKind::Unauthorized),
        Kind::Forbidden => Some(ErrorKind::Forbidden),
        Kind::Conflict => Some(ErrorKind::Conflict),
        Kind::RateLimit => Some(ErrorKind::RateLimit),
        Kind::Timeout => Some(ErrorKind::Timeout),
        Kind::Interrupted => Some(ErrorKind::Interrupted),
        Kind::Aborted => Some(ErrorKind::Aborted),
        Kind::Closed => Some(ErrorKind::Closed),
        Kind::NotAvailable => Some(ErrorKind::NotAvailable),
        Kind::Internal => Some(ErrorKind::Internal),
        Kind::BudgetExceeded => Some(ErrorKind::BudgetExceeded),
        Kind::PolicyDenied => Some(ErrorKind::PolicyDenied),
        Kind::Unspecified => None,
    }
}

/// The bearer token of a request.
pub fn bearer<T>(request: &Request<T>) -> Result<&str, Status> {
    request
        .metadata()
        .get("authorization")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.strip_prefix("Bearer "))
        .ok_or_else(|| Status::unauthenticated("missing bearer grant"))
}

/// Validate the request's grant for this endpoint and capability, and return
/// it. The caller still enforces its own scope.
pub fn authenticate<T>(
    request: &Request<T>,
    grants: &Grants,
    descriptor: &Endpoint,
    capability: &str,
) -> Result<Grant, Status> {
    let grant = grants.0.validate(bearer(request)?).map_err(status)?;
    descriptor.authorize(&grant).map_err(status)?;
    if grant.capability != capability {
        return Err(Status::permission_denied(format!(
            "grant is for {:?}, this endpoint serves {capability:?}",
            grant.capability
        )));
    }
    Ok(grant)
}

/// The grant previously inserted into the request extensions by the
/// interceptor.
pub fn grant_of<T>(request: &Request<T>) -> Result<Grant, Status> {
    request
        .extensions()
        .get::<Grant>()
        .cloned()
        .ok_or_else(|| Status::unauthenticated("missing grant"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn error_kinds_map_to_stable_codes() {
        assert_eq!(status(Error::not_found("nope")).code(), Code::NotFound);
        assert_eq!(
            status(Error::forbidden("no")).code(),
            Code::PermissionDenied
        );
        assert_eq!(
            status(Error::validation("bad")).code(),
            Code::InvalidArgument
        );
        assert_eq!(
            status(Error::not_available("later")).code(),
            Code::Unavailable
        );
        assert_eq!(
            status(Error::closed("gone")).code(),
            Code::FailedPrecondition
        );
        // The message is preserved verbatim.
        assert_eq!(status(Error::not_available("later")).message(), "later");
    }

    #[test]
    fn a_closed_handle_survives_the_round_trip() {
        let status = status(Error::closed("sandbox: session is closed"));
        assert_eq!(status.code(), Code::FailedPrecondition);
        assert_eq!(from_status(status).kind(), ErrorKind::Closed);
    }

    /// The regression that motivated the contract: two categories share a code,
    /// so a code-only round trip downgrades them.
    #[test]
    fn categories_that_share_a_code_still_arrive_intact() {
        let budget = status(Error::budget_exceeded("monthly budget spent"));
        assert_eq!(budget.code(), Code::ResourceExhausted);
        assert_eq!(from_status(budget).kind(), ErrorKind::BudgetExceeded);

        let denied = status(Error::policy_denied("blocked by eval policy"));
        assert_eq!(denied.code(), Code::PermissionDenied);
        assert_eq!(from_status(denied).kind(), ErrorKind::PolicyDenied);
    }

    #[test]
    fn every_kind_survives_the_round_trip() {
        for kind in ErrorKind::ALL {
            assert_ne!(
                kind_to_wire(kind),
                wire::Kind::Unspecified,
                "{kind} has no wire value; add it to kind_to_wire"
            );
            let status = status(Error::new(kind, "boom"));
            let decoded = from_status(status);
            assert_eq!(decoded.kind(), kind, "{kind} did not survive");
            assert_eq!(decoded.message(), "boom");
        }
    }

    #[test]
    fn details_retry_and_request_id_survive_the_round_trip() {
        let error = Error::not_available("provider is down")
            .with_detail(
                Detail::new("inference", "provider_truncated")
                    .with_message("stream ended without a finish event")
                    .with_field("field", "generate.input.content")
                    .with_field("detail", "stream.finish.missing"),
            )
            .with_retry(Retry::after(std::time::Duration::from_millis(1500)).with_attempts(2))
            .with_request_id("req-42");
        let decoded = from_status(status(error));

        assert_eq!(decoded.kind(), ErrorKind::NotAvailable);
        let detail = decoded.detail("inference").expect("detail");
        assert_eq!(detail.code(), "provider_truncated");
        assert_eq!(detail.message(), "stream ended without a finish event");
        assert_eq!(detail.field("detail"), Some("stream.finish.missing"));
        let retry = decoded.retry().expect("retry");
        assert_eq!(
            retry.backoff(),
            Some(std::time::Duration::from_millis(1500))
        );
        assert_eq!(retry.wire_attempts(), 2);
        assert_eq!(decoded.request_id(), Some("req-42"));
    }

    /// A peer that never learned the payload keeps working: the code is still
    /// there and still maps the way it always did.
    #[test]
    fn a_status_without_a_payload_falls_back_to_the_code() {
        let status = Status::new(Code::NotFound, "nope");
        let decoded = from_status(status);
        assert_eq!(decoded.kind(), ErrorKind::NotFound);
        assert_eq!(decoded.message(), "nope");
        assert_eq!(decoded.code(), None);

        // The two categories that share a code cannot be recovered without the
        // payload; the fallback picks the retryable reading, which is the
        // conservative one for a caller that only sees a status.
        let status = Status::new(Code::ResourceExhausted, "slow down");
        assert_eq!(from_status(status).kind(), ErrorKind::RateLimit);
    }

    /// A payload we cannot parse must not turn a request failure into an
    /// internal error: fall back to the code.
    #[test]
    fn an_unparseable_payload_falls_back_to_the_code() {
        let status = Status::with_details(
            Code::InvalidArgument,
            "bad",
            Bytes::from_static(&[0xff, 0xff, 0xff]),
        );
        let decoded = from_status(status);
        assert_eq!(decoded.kind(), ErrorKind::Validation);
        assert_eq!(decoded.message(), "bad");
    }

    #[test]
    fn a_payload_with_an_unknown_kind_falls_back_to_the_code() {
        let payload = wire::Error {
            kind: 9999,
            message: String::new(),
            details: Vec::new(),
            retry: None,
            request_id: String::new(),
        };
        let status = Status::with_details(
            Code::FailedPrecondition,
            "the message on the status",
            Bytes::from(payload.encode_to_vec()),
        );
        let decoded = from_status(status);
        assert_eq!(decoded.kind(), ErrorKind::Closed);
        // An empty payload message defers to the status message.
        assert_eq!(decoded.message(), "the message on the status");
    }

    #[test]
    fn an_empty_retry_block_is_not_carried() {
        let encoded = encode(&Error::not_available("later").with_retry(Retry::default()));
        let payload = decode(&encoded).expect("payload");
        assert!(payload.retry.is_none());
        assert_eq!(payload.kind, wire::Kind::NotAvailable as i32);
    }

    #[test]
    fn a_request_without_a_bearer_is_unauthenticated() {
        let request = Request::new(());
        let status = bearer(&request).unwrap_err();
        assert_eq!(status.code(), Code::Unauthenticated);
    }

    #[test]
    fn a_bearer_is_extracted() {
        let mut request = Request::new(());
        request
            .metadata_mut()
            .insert("authorization", "Bearer abc".parse().expect("metadata"));
        assert_eq!(bearer(&request).expect("bearer"), "abc");
    }
}

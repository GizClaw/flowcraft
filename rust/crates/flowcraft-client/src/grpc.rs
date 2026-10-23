//! Shared gRPC plumbing.
//!
//! Every capability endpoint speaks the same dialect: a bearer grant in
//! metadata, an endpoint/tenant/capability check, a scope check, and a mapping
//! from the platform's error taxonomy to a gRPC status. Keeping that here means
//! each capability crate only writes its own message handling.

use flowcraft_error::{Error, ErrorKind};
use tonic::{Code, Request, Status};

use flowcraft_kernel::catalog::Endpoint;
use flowcraft_kernel::grant::Grant;
use flowcraft_kernel::handles::Grants;

/// Map the platform's error taxonomy onto one gRPC status.
pub fn status(error: Error) -> Status {
    let code = match error.kind() {
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
    };
    Status::new(code, error.to_string())
}

/// Map a gRPC status back to the platform taxonomy, for client call sites.
#[must_use]
pub fn from_status(status: Status) -> Error {
    let kind = match status.code() {
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
        Code::Unknown if status.message().contains("transport error") => ErrorKind::NotAvailable,
        _ => ErrorKind::Internal,
    };
    Error::new(kind, status.message().to_owned())
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

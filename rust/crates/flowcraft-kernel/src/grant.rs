//! Grants: short-lived, session-scoped capability leases.
//!
//! A grant is an Ed25519-signed compact token. The endpoint validates it
//! offline — no introspection round trip, so the platform is not a single point
//! of failure on the data path. Revocation is local (this daemon); a farm of
//! daemons relies on the short TTL instead.

use std::collections::BTreeSet;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};
use flowcraft_error::{Error, Result};
use serde::{Deserialize, Serialize};
use serde_json::Value;

/// Injected clock, so expiry is testable.
pub type Clock = Arc<dyn Fn() -> SystemTime + Send + Sync>;

/// What a caller asks the issuer for.
#[derive(Debug, Clone)]
pub struct GrantRequest {
    /// The session the grant is bound to.
    pub session: String,
    /// The tenant the session belongs to.
    pub tenant: String,
    /// The capability, e.g. `workspace.Workspace/v1`.
    pub capability: String,
    /// The endpoint name it is valid at.
    pub endpoint: String,
    /// Capability-specific limits.
    pub scope: Value,
    /// Override the issuer's default TTL.
    pub ttl: Option<Duration>,
}

/// An issued lease.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Grant {
    /// The grant id (`jti`), used for revocation.
    pub id: String,
    /// The session.
    pub session: String,
    /// The tenant.
    pub tenant: String,
    /// The capability.
    pub capability: String,
    /// The endpoint.
    pub endpoint: String,
    /// Capability-specific limits, frozen at issue time.
    pub scope: Value,
    /// Unix seconds.
    pub issued_at: u64,
    /// Unix seconds.
    pub expires_at: u64,
    /// The signed token to present on the wire.
    pub token: String,
}

impl Grant {
    /// Whether the grant is still valid at `now`.
    #[must_use]
    pub fn is_live(&self, now: SystemTime) -> bool {
        unix_seconds(now) < self.expires_at
    }
}

/// The issuer contract.
pub trait GrantIssuer: Send + Sync {
    /// Mint a grant.
    fn issue(&self, request: &GrantRequest) -> Result<Grant>;
    /// Validate a token and return its grant.
    fn validate(&self, token: &str) -> Result<Grant>;
    /// Revoke one grant.
    fn revoke(&self, id: &str) -> Result<()>;
    /// Revoke every grant of a session (close, crash, drain).
    fn revoke_session(&self, session: &str) -> Result<()>;
}

#[derive(Debug, Serialize, Deserialize)]
struct Claims {
    jti: String,
    sub: String,
    tenant: String,
    cap: String,
    aud: String,
    scope: Value,
    iat: u64,
    exp: u64,
}

/// In-memory issuer with an Ed25519 key.
pub struct MemoryIssuer {
    signing: SigningKey,
    verifying: VerifyingKey,
    ttl: Duration,
    leeway: Duration,
    clock: Clock,
    revoked: Mutex<BTreeSet<String>>,
    revoked_sessions: Mutex<BTreeSet<String>>,
}

impl std::fmt::Debug for MemoryIssuer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("MemoryIssuer")
            .field("ttl", &self.ttl)
            .finish_non_exhaustive()
    }
}

impl MemoryIssuer {
    /// Generate a fresh key.
    #[must_use]
    pub fn new(ttl: Duration) -> Self {
        Self::from_key(SigningKey::generate(&mut rand::rngs::OsRng), ttl)
    }

    /// Deterministic key, for tests and for a key loaded from a secret.
    #[must_use]
    pub fn from_seed(seed: [u8; 32], ttl: Duration) -> Self {
        Self::from_key(SigningKey::from_bytes(&seed), ttl)
    }

    fn from_key(signing: SigningKey, ttl: Duration) -> Self {
        Self {
            verifying: signing.verifying_key(),
            signing,
            ttl,
            leeway: Duration::from_secs(5),
            clock: Arc::new(SystemTime::now),
            revoked: Mutex::new(BTreeSet::new()),
            revoked_sessions: Mutex::new(BTreeSet::new()),
        }
    }

    /// Replace the clock (tests).
    #[must_use]
    pub fn with_clock(mut self, clock: Clock) -> Self {
        self.clock = clock;
        self
    }

    /// The default lifetime.
    #[must_use]
    pub fn ttl(&self) -> Duration {
        self.ttl
    }

    fn now(&self) -> SystemTime {
        (self.clock)()
    }

    fn check_not_revoked(&self, claims: &Claims) -> Result<()> {
        if self
            .revoked_sessions
            .lock()
            .map_err(|_| Error::internal("grant revocation lock poisoned"))?
            .contains(&claims.sub)
        {
            return Err(Error::forbidden(format!(
                "grant for session {:?} was revoked",
                claims.sub
            )));
        }
        if self
            .revoked
            .lock()
            .map_err(|_| Error::internal("grant revocation lock poisoned"))?
            .contains(&claims.jti)
        {
            return Err(Error::forbidden(format!(
                "grant {:?} was revoked",
                claims.jti
            )));
        }
        Ok(())
    }
}

impl GrantIssuer for MemoryIssuer {
    fn issue(&self, request: &GrantRequest) -> Result<Grant> {
        for (field, value) in [
            ("session", &request.session),
            ("capability", &request.capability),
            ("endpoint", &request.endpoint),
        ] {
            if value.trim().is_empty() {
                return Err(Error::validation(format!(
                    "grant request: {field} is empty"
                )));
            }
        }
        let ttl = request.ttl.unwrap_or(self.ttl);
        if ttl.is_zero() {
            return Err(Error::validation("grant request: ttl must be positive"));
        }
        let issued_at = unix_seconds(self.now());
        let claims = Claims {
            jti: format!("g-{:016x}", rand::random::<u64>()),
            sub: request.session.clone(),
            tenant: request.tenant.clone(),
            cap: request.capability.clone(),
            aud: request.endpoint.clone(),
            scope: request.scope.clone(),
            iat: issued_at,
            exp: issued_at.saturating_add(ttl.as_secs()),
        };

        let payload = serde_json::to_vec(&claims)
            .map_err(|error| Error::internal(format!("grant claims: {error}")))?;
        let encoded = URL_SAFE_NO_PAD.encode(&payload);
        let signature = self.signing.sign(encoded.as_bytes());
        let token = format!("{encoded}.{}", URL_SAFE_NO_PAD.encode(signature.to_bytes()));

        Ok(Grant {
            id: claims.jti,
            session: claims.sub,
            tenant: claims.tenant,
            capability: claims.cap,
            endpoint: claims.aud,
            scope: claims.scope,
            issued_at: claims.iat,
            expires_at: claims.exp,
            token,
        })
    }

    fn validate(&self, token: &str) -> Result<Grant> {
        let (encoded, signature) = token
            .split_once('.')
            .ok_or_else(|| Error::unauthorized("grant token is malformed"))?;
        let signature_bytes = URL_SAFE_NO_PAD
            .decode(signature)
            .map_err(|_| Error::unauthorized("grant signature is not base64url"))?;
        let signature = Signature::from_slice(&signature_bytes)
            .map_err(|_| Error::unauthorized("grant signature has the wrong length"))?;
        self.verifying
            .verify(encoded.as_bytes(), &signature)
            .map_err(|_| Error::unauthorized("grant signature does not verify"))?;

        let payload = URL_SAFE_NO_PAD
            .decode(encoded)
            .map_err(|_| Error::unauthorized("grant payload is not base64url"))?;
        let claims: Claims = serde_json::from_slice(&payload)
            .map_err(|error| Error::unauthorized(format!("grant payload: {error}")))?;

        self.check_not_revoked(&claims)?;

        let now = unix_seconds(self.now());
        if now.saturating_add(self.leeway.as_secs()) >= claims.exp {
            return Err(Error::unauthorized(format!(
                "grant for session {:?} expired",
                claims.sub
            )));
        }

        Ok(Grant {
            id: claims.jti,
            session: claims.sub,
            tenant: claims.tenant,
            capability: claims.cap,
            endpoint: claims.aud,
            scope: claims.scope,
            issued_at: claims.iat,
            expires_at: claims.exp,
            token: token.to_owned(),
        })
    }

    fn revoke(&self, id: &str) -> Result<()> {
        self.revoked
            .lock()
            .map_err(|_| Error::internal("grant revocation lock poisoned"))?
            .insert(id.to_owned());
        Ok(())
    }

    fn revoke_session(&self, session: &str) -> Result<()> {
        self.revoked_sessions
            .lock()
            .map_err(|_| Error::internal("grant revocation lock poisoned"))?
            .insert(session.to_owned());
        Ok(())
    }
}

fn unix_seconds(time: SystemTime) -> u64 {
    time.duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_secs())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request(session: &str) -> GrantRequest {
        GrantRequest {
            session: session.to_owned(),
            tenant: "a".to_owned(),
            capability: "workspace.Workspace/v1".to_owned(),
            endpoint: "ws".to_owned(),
            scope: serde_json::json!({"root": "repos"}),
            ttl: None,
        }
    }

    #[test]
    fn issue_then_validate_round_trips() {
        let issuer = MemoryIssuer::new(Duration::from_secs(60));
        let grant = issuer.issue(&request("s-1")).unwrap();
        assert_eq!(grant.session, "s-1");
        let recovered = issuer.validate(&grant.token).unwrap();
        assert_eq!(recovered.id, grant.id);
        assert_eq!(recovered.scope, grant.scope);
    }

    #[test]
    fn a_tampered_payload_is_rejected() {
        let issuer = MemoryIssuer::new(Duration::from_secs(60));
        let grant = issuer.issue(&request("s-1")).unwrap();
        let (payload, signature) = grant.token.split_once('.').unwrap();
        let mut bytes = URL_SAFE_NO_PAD.decode(payload).unwrap();
        bytes[0] ^= 0xff;
        let tampered = format!("{}.{}", URL_SAFE_NO_PAD.encode(bytes), signature);
        let error = issuer.validate(&tampered).unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Unauthorized);
    }

    #[test]
    fn expiry_uses_the_injected_clock() {
        let start = UNIX_EPOCH + Duration::from_secs(1_000);
        let now = Arc::new(Mutex::new(start));
        let read = now.clone();
        let issuer = MemoryIssuer::new(Duration::from_secs(60))
            .with_clock(Arc::new(move || *read.lock().unwrap()));
        let grant = issuer.issue(&request("s-1")).unwrap();
        assert!(issuer.validate(&grant.token).is_ok());
        *now.lock().unwrap() = start + Duration::from_secs(120);
        let error = issuer.validate(&grant.token).unwrap_err();
        assert!(error.to_string().contains("expired"), "{error}");
    }

    #[test]
    fn revocation_is_per_grant_and_per_session() {
        let issuer = MemoryIssuer::new(Duration::from_secs(60));
        let first = issuer.issue(&request("s-1")).unwrap();
        let second = issuer.issue(&request("s-1")).unwrap();
        issuer.revoke(&first.id).unwrap();
        assert!(issuer.validate(&first.token).is_err());
        assert!(issuer.validate(&second.token).is_ok());
        issuer.revoke_session("s-1").unwrap();
        assert!(issuer.validate(&second.token).is_err());
    }

    #[test]
    fn empty_request_fields_are_rejected() {
        let issuer = MemoryIssuer::new(Duration::from_secs(60));
        let mut bad = request("s-1");
        bad.endpoint.clear();
        assert_eq!(
            issuer.issue(&bad).unwrap_err().kind(),
            flowcraft_error::ErrorKind::Validation
        );
    }
}

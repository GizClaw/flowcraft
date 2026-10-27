//! Grants: short-lived, session-scoped capability leases.
//!
//! A grant is an Ed25519-signed compact token. The endpoint validates it
//! offline — no introspection round trip, so the platform is not a single point
//! of failure on the data path. Revocation is local (this daemon); a farm of
//! daemons relies on the short TTL instead.
//!
//! Revocation is therefore *bounded by expiry*: a revoked grant is remembered
//! only until the second it would have stopped working anyway, and a revoked
//! session until the last grant it was given expires. The table is pruned
//! whenever it is read or written, so a daemon that revokes a session per run
//! does not carry a log of every run it ever served.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use crate::error::{Error, Result};
use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};
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

/// How many live grants this issuer tracks before it sweeps the expired ones.
///
/// The map only has to stay bounded, not empty: sweeping on every `issue` would
/// put an O(n) pass on a path that runs per lease renewal.
const LIVE_GRANTS_PRUNE_AT: usize = 1024;

/// In-memory issuer with an Ed25519 key.
pub struct MemoryIssuer {
    signing: SigningKey,
    verifying: VerifyingKey,
    ttl: Duration,
    leeway: Duration,
    clock: Clock,
    /// What this issuer minted and has not yet seen expire: the expiries a
    /// revocation is dropped at.
    issued: Mutex<Issued>,
    /// The revocations still being honoured.
    revocations: Mutex<Revocations>,
}

/// What this issuer has minted and not yet seen expire.
///
/// Both maps exist so a revocation can be dropped at the *right* second instead
/// of a guessed one: `revoke(id)` needs that grant's `exp`, and
/// `revoke_session` needs the latest `exp` among the session's grants. They are
/// pruned by expiry, so the memory a long-lived daemon holds is proportional to
/// the grants that are still valid, not to the grants it ever issued.
#[derive(Debug, Default)]
struct Issued {
    /// `jti` -> the second it stops being valid.
    grants: HashMap<String, u64>,
    /// session -> the latest `exp` among its grants.
    sessions: HashMap<String, u64>,
    /// The largest `exp` ever minted here.
    ///
    /// The fallback window for a name this issuer does not know (a restart, an
    /// id minted by another process) must not be shorter than a grant that is
    /// still valid.
    high_water: u64,
}

impl Issued {
    fn prune(&mut self, now: u64) {
        self.grants.retain(|_, exp| *exp > now);
        self.sessions.retain(|_, exp| *exp > now);
    }
}

/// One entry in the pruning index.
#[derive(Debug)]
enum Revoked {
    Grant(String),
    Session(String),
}

/// The revocations this issuer is still honouring.
///
/// A revocation is only interesting while the grant could otherwise be accepted,
/// so every entry carries the second it may be dropped at, and pruning is a
/// range scan of [`Revocations::by_exp`] rather than a walk of every name ever
/// revoked.
#[derive(Debug, Default)]
struct Revocations {
    grants: HashSet<String>,
    sessions: HashSet<String>,
    /// The second an entry may be dropped -> the entries that expire then.
    by_exp: BTreeMap<u64, Vec<Revoked>>,
}

impl Revocations {
    /// Drop everything whose window has closed at `now`.
    fn prune(&mut self, now: u64) {
        // `split_off(k)` keeps the keys at or after `k`: keeping `now + 1` and
        // above drops exactly the entries whose `exp` is at or before `now`.
        let live = self.by_exp.split_off(&now.saturating_add(1));
        for (_, entries) in std::mem::replace(&mut self.by_exp, live) {
            for entry in entries {
                match entry {
                    Revoked::Grant(id) => {
                        self.grants.remove(&id);
                    }
                    Revoked::Session(session) => {
                        self.sessions.remove(&session);
                    }
                }
            }
        }
    }

    /// Whether this token was revoked, pruning expired entries first.
    fn check(&mut self, now: u64, id: &str, session: &str) -> Result<()> {
        self.prune(now);
        if self.sessions.contains(session) {
            return Err(Error::forbidden(format!(
                "grant for session {session:?} was revoked"
            )));
        }
        if self.grants.contains(id) {
            return Err(Error::forbidden(format!("grant {id:?} was revoked")));
        }
        Ok(())
    }

    fn revoke_grant(&mut self, now: u64, id: &str, exp: u64) {
        self.prune(now);
        if self.grants.insert(id.to_owned()) {
            self.by_exp
                .entry(exp.max(now))
                .or_default()
                .push(Revoked::Grant(id.to_owned()));
        }
    }

    fn revoke_session(&mut self, now: u64, session: &str, exp: u64) {
        self.prune(now);
        if self.sessions.insert(session.to_owned()) {
            self.by_exp
                .entry(exp.max(now))
                .or_default()
                .push(Revoked::Session(session.to_owned()));
        }
    }
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
            issued: Mutex::new(Issued::default()),
            revocations: Mutex::new(Revocations::default()),
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
        let now = unix_seconds(self.now());
        self.revocations
            .lock()
            .map_err(|_| Error::internal("grant revocation lock is poisoned"))?
            .check(now, &claims.jti, &claims.sub)
    }

    /// The second a revocation of something this issuer cannot look up may be
    /// dropped at: the default lifetime, or the longest grant ever minted here,
    /// whichever is later.
    fn retention(&self, now: u64, issued: &Issued) -> u64 {
        now.saturating_add(self.ttl.as_secs())
            .max(issued.high_water)
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

        // Record the expiry before the token can leave: a revocation that races
        // this call must still know when it may be dropped, and a session's
        // revocation has to cover the longest-lived grant it was given.
        {
            let mut issued = self
                .issued
                .lock()
                .map_err(|_| Error::internal("grant issue lock is poisoned"))?;
            issued.high_water = issued.high_water.max(claims.exp);
            issued.grants.insert(claims.jti.clone(), claims.exp);
            let latest = issued
                .sessions
                .entry(claims.sub.clone())
                .or_insert(claims.exp);
            *latest = (*latest).max(claims.exp);
            // Amortised, not per call: the hot path is `validate`, and this map
            // only has to stay bounded, not empty.
            if issued.grants.len() > LIVE_GRANTS_PRUNE_AT {
                issued.prune(issued_at);
            }
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
        let now = unix_seconds(self.now());
        // An id this issuer does not know (a restart, another process's grant)
        // keeps the conservative window rather than a guess that could drop the
        // revocation while the token is still valid.
        let exp = {
            let issued = self
                .issued
                .lock()
                .map_err(|_| Error::internal("grant issue lock is poisoned"))?;
            issued
                .grants
                .get(id)
                .copied()
                .unwrap_or_else(|| self.retention(now, &issued))
        };
        self.revocations
            .lock()
            .map_err(|_| Error::internal("grant revocation lock is poisoned"))?
            .revoke_grant(now, id, exp);
        Ok(())
    }

    fn revoke_session(&self, session: &str) -> Result<()> {
        let now = unix_seconds(self.now());
        // The whole session stays revoked until its *last* grant expires: a
        // grant minted with a longer TTL than the issuer default must not
        // outlive the revocation.
        let exp = {
            let issued = self
                .issued
                .lock()
                .map_err(|_| Error::internal("grant issue lock is poisoned"))?;
            issued
                .sessions
                .get(session)
                .copied()
                .unwrap_or_else(|| self.retention(now, &issued))
        };
        self.revocations
            .lock()
            .map_err(|_| Error::internal("grant revocation lock is poisoned"))?
            .revoke_session(now, session, exp);
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
        assert_eq!(error.kind(), crate::error::ErrorKind::Unauthorized);
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

    /// An issuer whose clock the test moves, so expiry is observable without
    /// sleeping.
    fn clocked(ttl: Duration, start: u64) -> (MemoryIssuer, Arc<Mutex<SystemTime>>) {
        let now = Arc::new(Mutex::new(UNIX_EPOCH + Duration::from_secs(start)));
        let read = now.clone();
        let issuer = MemoryIssuer::new(ttl).with_clock(Arc::new(move || *read.lock().unwrap()));
        (issuer, now)
    }

    /// The table is not a log of everything ever revoked: an entry is only worth
    /// keeping while the grant could otherwise be accepted.
    #[test]
    fn revocations_do_not_accumulate_past_their_expiry() {
        let (issuer, now) = clocked(Duration::from_secs(60), 1_000);
        for _ in 0..200 {
            let grant = issuer.issue(&request("s-1")).unwrap();
            issuer.revoke(&grant.id).unwrap();
        }
        assert_eq!(issuer.revocations.lock().unwrap().grants.len(), 200);

        // An hour later one more revocation sweeps them all: the 200 grants
        // expired at +60 s, so nothing needs to remember them.
        *now.lock().unwrap() = UNIX_EPOCH + Duration::from_secs(3_600);
        let fresh = issuer.issue(&request("s-2")).unwrap();
        issuer.revoke(&fresh.id).unwrap();
        let table = issuer.revocations.lock().unwrap();
        assert_eq!(table.grants.len(), 1, "expired revocations are dropped");
        assert_eq!(table.by_exp.len(), 1, "and so is their index entry");
        assert!(table.sessions.is_empty());
    }

    /// A session's revocation has to cover its *longest-lived* grant, not the
    /// issuer's default TTL: a lease minted with a longer TTL must not outlive
    /// the revocation.
    #[test]
    fn a_session_revocation_outlives_a_grant_with_a_longer_ttl() {
        let (issuer, now) = clocked(Duration::from_secs(60), 1_000);
        let short = issuer.issue(&request("s-1")).unwrap();
        let mut longer = request("s-1");
        longer.ttl = Some(Duration::from_secs(600));
        let longer = issuer.issue(&longer).unwrap();
        assert!(longer.expires_at > short.expires_at);
        issuer.revoke_session("s-1").unwrap();

        // Past the short grant and past the default TTL: still revoked.
        *now.lock().unwrap() = UNIX_EPOCH + Duration::from_secs(1_300);
        let minted = issuer.issue(&request("s-1")).unwrap();
        assert_eq!(
            issuer.validate(&minted.token).unwrap_err().kind(),
            crate::error::ErrorKind::Forbidden
        );

        // Past the long grant too: nothing is left to protect, so the session is
        // usable again (a new session id would also be, but the point is that the
        // entry does not stay forever).
        *now.lock().unwrap() = UNIX_EPOCH + Duration::from_secs(1_700);
        let later = issuer.issue(&request("s-1")).unwrap();
        assert!(issuer.validate(&later.token).is_ok());
    }

    /// An id this issuer never minted (a restart, another process's grant, a
    /// grant it already pruned) keeps a conservative window instead of a guess.
    #[test]
    fn an_unknown_id_is_revoked_for_the_conservative_window() {
        let (issuer, now) = clocked(Duration::from_secs(60), 1_000);
        issuer.revoke("g-elsewhere").unwrap();

        *now.lock().unwrap() = UNIX_EPOCH + Duration::from_secs(1_030);
        let inside = issuer.issue(&request("s-1")).unwrap();
        assert!(
            issuer.validate(&inside.token).is_ok(),
            "revoking an unknown id must not disturb other grants"
        );
        assert!(
            issuer
                .revocations
                .lock()
                .unwrap()
                .grants
                .contains("g-elsewhere"),
            "still inside the window"
        );

        // One second past the default TTL, a validation prunes it.
        *now.lock().unwrap() = UNIX_EPOCH + Duration::from_secs(1_061);
        let fresh = issuer.issue(&request("s-1")).unwrap();
        assert!(issuer.validate(&fresh.token).is_ok());
        assert!(
            !issuer
                .revocations
                .lock()
                .unwrap()
                .grants
                .contains("g-elsewhere")
        );
    }

    /// The live index behind those windows is swept on a threshold, so a daemon
    /// that mints a lease per request does not keep an entry per lease forever.
    #[test]
    fn the_live_index_is_bounded_by_the_prune_threshold() {
        let (issuer, now) = clocked(Duration::from_secs(1), 1_000);
        for _ in 0..(LIVE_GRANTS_PRUNE_AT + 10) {
            issuer.issue(&request("s-1")).unwrap();
        }
        assert!(issuer.issued.lock().unwrap().grants.len() > LIVE_GRANTS_PRUNE_AT);

        // Long after they expired, one more issue crosses the threshold and
        // sweeps them.
        *now.lock().unwrap() = UNIX_EPOCH + Duration::from_secs(2_000);
        issuer.issue(&request("s-2")).unwrap();
        let issued = issuer.issued.lock().unwrap();
        assert!(
            issued.grants.len() <= 2,
            "{} live entries after the sweep",
            issued.grants.len()
        );
        assert!(
            issued.sessions.len() <= 1,
            "{} sessions",
            issued.sessions.len()
        );
    }

    #[test]
    fn empty_request_fields_are_rejected() {
        let issuer = MemoryIssuer::new(Duration::from_secs(60));
        let mut bad = request("s-1");
        bad.endpoint.clear();
        assert_eq!(
            issuer.issue(&bad).unwrap_err().kind(),
            crate::error::ErrorKind::Validation
        );
    }
}

//! `net/mitm/ca.go`: the temporary authority a proxy run terminates TLS with.
//!
//! Every run mints a fresh ECDSA P-256 root, keeps its key in memory and never
//! writes it anywhere; the root's one public appearance is [`Ca::pem`], which
//! the sandbox bundle carries so the clients behind the proxy trust what it
//! signs. Leaves are minted per host on demand and cached, so a client opening
//! fifty connections to one host pays for one key generation.
//!
//! Three details are core's, and each is a decision rather than an
//! implementation note:
//!
//! - **A name gets a DNS SAN, a literal gets an IP SAN.** A certificate for
//!   `127.0.0.1` whose only SAN is a DNS name is refused by every verifier that
//!   checks the SAN type, so a `CONNECT 127.0.0.1:443` would fail inside the
//!   sandbox while working on the host.
//! - **The root is backdated an hour and lives a day**, and a leaf expires an
//!   hour before its signer does. The backdate is what keeps a machine whose
//!   clock drifted behind from refusing a certificate minted a moment ago.
//! - **The cache is bounded and FIFO.** A proxy that sees many hostnames must
//!   not grow a map without end, and an entry is cheap to regenerate, so
//!   recency is not worth the bookkeeping.
//!
//! # What a caller can tell apart from core
//!
//! The certificates are not byte-identical -- Go's `crypto/x509` and rcgen order
//! extensions differently, and neither DER form is a contract -- but the
//! properties core's callers rely on are pinned by the tests below: the chain is
//! `[leaf, root]`, the SAN type follows the host, and a rustls client whose root
//! store holds only this CA completes a handshake for both a name and a
//! literal.

use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex, MutexGuard};

use flowcraft_contract::error::{Error, ErrorKind};
use rcgen::{
    BasicConstraints, CertificateParams, CertifiedIssuer, DistinguishedName, DnType,
    ExtendedKeyUsagePurpose, IsCa, KeyPair, KeyUsagePurpose, PKCS_ECDSA_P256_SHA256, SanType,
    SerialNumber,
};
use rustls::ServerConfig;
use rustls::crypto::CryptoProvider;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use time::{Duration, OffsetDateTime};

/// How long the ephemeral root lives (`caValidity`).
const VALIDITY_SECS: i64 = 24 * 60 * 60;

/// How much earlier than the root a leaf expires, so it never outlives its
/// signer.
const LEAF_SHORTFALL_SECS: i64 = 60 * 60;

/// How far both are backdated, so a clock a little behind still verifies.
const BACKDATE_SECS: i64 = 60 * 60;

/// `maxCachedLeaves`: the per-CA bound on minted leaves.
const MAX_CACHED_LEAVES: usize = 1024;

/// A per-run temporary certificate authority.
///
/// The key lives only in this value and is dropped with it. Leaves are cached
/// as whole [`ServerConfig`]s, which is both the form the serving side needs and
/// the expensive part (a key pair plus a signature) of one.
pub struct Ca {
    issuer: CertifiedIssuer<'static, KeyPair>,
    provider: Arc<CryptoProvider>,
    root: CertificateDer<'static>,
    pem: String,
    cache: Mutex<Cache>,
}

/// The FIFO cache of minted server configs, ordered by insertion.
#[derive(Default)]
struct Cache {
    configs: HashMap<String, Arc<ServerConfig>>,
    order: VecDeque<String>,
}

impl Ca {
    /// Generate a fresh self-signed ECDSA P-256 root.
    ///
    /// The provider is rustls' `ring` one, which is also what
    /// [`server_config`](Ca::server_config) builds with: the serial numbers come
    /// from the same entropy source as the keys, and a caller that installs this
    /// provider as rustls' process default gets one implementation on both sides
    /// of every handshake.
    pub fn new() -> Result<Ca, Error> {
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let key = KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256)
            .map_err(|err| internal(format!("mitm: generate CA key: {err}")))?;
        let mut params = CertificateParams::default();
        params.distinguished_name = common_name("flowcraft netproxy temporary CA");
        params.serial_number = Some(serial(provider.as_ref())?);
        params.not_before = backdated();
        params.not_after = ahead(VALIDITY_SECS);
        params.key_usages = vec![
            KeyUsagePurpose::KeyCertSign,
            KeyUsagePurpose::DigitalSignature,
        ];
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        let issuer = CertifiedIssuer::self_signed(params, key)
            .map_err(|err| internal(format!("mitm: create CA certificate: {err}")))?;
        let root = issuer.der().clone();
        let pem = issuer.pem();
        Ok(Ca {
            issuer,
            provider,
            root,
            pem,
            cache: Mutex::new(Cache::default()),
        })
    }

    /// The root certificate in PEM form, for bundle injection into the sandbox.
    pub fn pem(&self) -> &str {
        &self.pem
    }

    /// The root certificate in DER form, which is what a root store takes.
    pub fn root(&self) -> &CertificateDer<'static> {
        &self.root
    }

    /// A server config whose leaf covers `host` and which offers `alpn` in
    /// order.
    ///
    /// This is core's `tls.Server` config -- ALPN, no client certificate, TLS
    /// 1.2 and up -- built around a cached leaf. The cache is keyed by host and
    /// checks the ALPN list too, because a config built for a different protocol
    /// set would negotiate the wrong one.
    pub fn server_config(&self, host: &str, alpn: &[&[u8]]) -> Result<Arc<ServerConfig>, Error> {
        let protocols: Vec<Vec<u8>> = alpn.iter().map(|protocol| protocol.to_vec()).collect();
        if let Some(config) = self.cached(host, &protocols) {
            return Ok(config);
        }
        let config = Arc::new(self.build(host, protocols)?);
        let mut cache = self.lock();
        // A host cached for another ALPN list is re-inserted at the back, so the
        // FIFO holds one entry per host rather than one per miss.
        cache.order.retain(|cached| cached != host);
        if cache.configs.len() >= MAX_CACHED_LEAVES
            && let Some(oldest) = cache.order.pop_front()
        {
            cache.configs.remove(&oldest);
        }
        cache.order.push_back(host.to_owned());
        cache.configs.insert(host.to_owned(), Arc::clone(&config));
        Ok(config)
    }

    fn cached(&self, host: &str, protocols: &[Vec<u8>]) -> Option<Arc<ServerConfig>> {
        let cache = self.lock();
        let config = cache.configs.get(host)?;
        (config.alpn_protocols == protocols).then(|| Arc::clone(config))
    }

    fn build(&self, host: &str, protocols: Vec<Vec<u8>>) -> Result<ServerConfig, Error> {
        let (chain, key) = self.mint_leaf(host)?;
        let mut config = ServerConfig::builder_with_provider(Arc::clone(&self.provider))
            // rustls' safe defaults are TLS 1.2 and 1.3, which is core's
            // `MinVersion: tls.VersionTLS12` with nothing older to offer.
            .with_safe_default_protocol_versions()
            .map_err(|err| internal(format!("mitm: protocol versions: {err}")))?
            .with_no_client_auth()
            .with_single_cert(chain, key)
            .map_err(|err| internal(format!("mitm: server config for {host}: {err}")))?;
        config.alpn_protocols = protocols;
        Ok(config)
    }

    /// `CA.issueLeaf`: a leaf for `host`, chained to this root.
    fn mint_leaf(
        &self,
        host: &str,
    ) -> Result<(Vec<CertificateDer<'static>>, PrivateKeyDer<'static>), Error> {
        let key = KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256)
            .map_err(|err| internal(format!("mitm: generate leaf key: {err}")))?;
        let mut params = CertificateParams::default();
        params.serial_number = Some(serial(self.provider.as_ref())?);
        params.distinguished_name = common_name(host);
        params.not_before = backdated();
        params.not_after = ahead(VALIDITY_SECS - LEAF_SHORTFALL_SECS);
        params.key_usages = vec![
            KeyUsagePurpose::DigitalSignature,
            KeyUsagePurpose::KeyEncipherment,
        ];
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        params.subject_alt_names = vec![san(host)?];
        let leaf = params
            .signed_by(&key, &self.issuer)
            .map_err(|err| internal(format!("mitm: issue leaf for {host:?}: {err}")))?;
        let chain = vec![leaf.der().clone(), self.root.clone()];
        Ok((
            chain,
            PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
        ))
    }

    fn lock(&self) -> MutexGuard<'_, Cache> {
        // A panic elsewhere does not make a cache of certificates wrong, only
        // possibly stale, and a stale entry is still a valid one.
        self.cache
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }
}

/// The subject core sets: one common name and nothing else.
fn common_name(name: &str) -> DistinguishedName {
    let mut subject = DistinguishedName::new();
    subject.push(DnType::CommonName, name);
    subject
}

/// The SAN a host deserves: a literal goes in `IPAddress`, everything else in
/// `dNSName`. See the module docs for why this is not a detail.
fn san(host: &str) -> Result<SanType, Error> {
    if let Ok(address) = host.parse::<std::net::IpAddr>() {
        return Ok(SanType::IpAddress(address));
    }
    let name = host
        .to_owned()
        .try_into()
        .map_err(|err| internal(format!("mitm: host {host:?} is not an IA5 name: {err}")))?;
    Ok(SanType::DnsName(name))
}

/// A 128-bit random serial, the width core draws from `crypto/rand`.
fn serial(provider: &CryptoProvider) -> Result<SerialNumber, Error> {
    let mut bytes = [0u8; 16];
    provider
        .secure_random
        .fill(&mut bytes)
        .map_err(|err| internal(format!("mitm: random serial: {err:?}")))?;
    Ok(SerialNumber::from_slice(&bytes))
}

fn backdated() -> OffsetDateTime {
    OffsetDateTime::now_utc() - Duration::seconds(BACKDATE_SECS)
}

fn ahead(seconds: i64) -> OffsetDateTime {
    OffsetDateTime::now_utc() + Duration::seconds(seconds)
}

fn internal(message: impl Into<String>) -> Error {
    Error::new(ErrorKind::Internal, message.into())
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use rustls::pki_types::ServerName;
    use rustls::{ClientConfig, ClientConnection, RootCertStore, ServerConfig, ServerConnection};

    use super::Ca;

    fn ca() -> Ca {
        match Ca::new() {
            Ok(ca) => ca,
            Err(err) => panic!("the authority should mint: {err}"),
        }
    }

    fn config(ca: &Ca, host: &str, alpn: &[&[u8]]) -> Arc<ServerConfig> {
        match ca.server_config(host, alpn) {
            Ok(config) => config,
            Err(err) => panic!("a config for {host} should build: {err}"),
        }
    }

    fn name(host: &str) -> ServerName<'static> {
        match ServerName::try_from(host.to_owned()) {
            Ok(name) => name,
            Err(err) => panic!("{host} should be a server name: {err}"),
        }
    }

    fn address(host: &str) -> ServerName<'static> {
        match host.parse::<std::net::IpAddr>() {
            Ok(ip) => ServerName::IpAddress(ip.into()),
            Err(err) => panic!("{host} should parse as an address: {err}"),
        }
    }

    /// A client whose only root is the CA under test.
    fn client(ca: &Ca) -> ClientConfig {
        let mut roots = RootCertStore::empty();
        if let Err(err) = roots.add(ca.root().clone()) {
            panic!("the root should load: {err}");
        }
        match ClientConfig::builder_with_provider(
            Arc::new(rustls::crypto::ring::default_provider()),
        )
        .with_safe_default_protocol_versions()
        {
            Ok(builder) => builder.with_root_certificates(roots).with_no_client_auth(),
            Err(err) => panic!("the provider should offer versions: {err}"),
        }
    }

    /// Run a handshake far enough to know both sides accepted, and return the
    /// client connection so a test can read what it authenticated.
    fn handshake(
        ca: &Ca,
        host: &str,
        server_name: ServerName<'static>,
    ) -> Result<ClientConnection, String> {
        let server_config = ca
            .server_config(host, &[b"h2", b"http/1.1"])
            .map_err(|err| err.to_string())?;
        let mut server = ServerConnection::new(server_config).map_err(|err| err.to_string())?;
        let mut client = ClientConnection::new(Arc::new(client(ca)), server_name)
            .map_err(|err| err.to_string())?;
        for _ in 0..8 {
            let mut wire = Vec::new();
            while client.wants_write() {
                client.write_tls(&mut wire).map_err(|err| err.to_string())?;
            }
            let mut read = 0;
            while read < wire.len() {
                read += server
                    .read_tls(&mut &wire[read..])
                    .map_err(|err| err.to_string())?;
            }
            server
                .process_new_packets()
                .map_err(|err| err.to_string())?;
            let mut wire = Vec::new();
            while server.wants_write() {
                server.write_tls(&mut wire).map_err(|err| err.to_string())?;
            }
            let mut read = 0;
            while read < wire.len() {
                read += client
                    .read_tls(&mut &wire[read..])
                    .map_err(|err| err.to_string())?;
            }
            client
                .process_new_packets()
                .map_err(|err| err.to_string())?;
            if !client.is_handshaking() && !server.is_handshaking() {
                return Ok(client);
            }
        }
        Err("handshake did not finish".to_owned())
    }

    fn verified(ca: &Ca, host: &str, server_name: ServerName<'static>) -> ClientConnection {
        match handshake(ca, host, server_name) {
            Ok(client) => client,
            Err(err) => panic!("the handshake for {host} should verify: {err}"),
        }
    }

    #[test]
    fn the_root_is_a_pem_certificate() {
        let ca = ca();
        assert!(ca.pem().starts_with("-----BEGIN CERTIFICATE-----"));
        assert!(ca.pem().trim_end().ends_with("-----END CERTIFICATE-----"));
    }

    #[test]
    fn a_name_leaf_verifies_against_the_root() {
        let ca = ca();
        let client = verified(&ca, "example.test", name("example.test"));
        let chain = match client.peer_certificates() {
            Some(chain) => chain,
            None => panic!("the server should have sent a chain"),
        };
        assert_eq!(chain.len(), 2, "leaf plus root, the way core sends them");
    }

    /// The property a DNS-SAN-for-everything authority does not have: a literal
    /// gets an IP SAN, so a verifier that checks the SAN type accepts it.
    #[test]
    fn a_literal_leaf_verifies_against_the_root() {
        let ca = ca();
        verified(&ca, "127.0.0.1", address("127.0.0.1"));
    }

    /// A name leaf must not satisfy a literal, or the branch above would be
    /// tested by nothing.
    #[test]
    fn a_name_leaf_does_not_cover_a_literal() {
        let ca = ca();
        assert!(handshake(&ca, "example.test", address("127.0.0.1")).is_err());
    }

    #[test]
    fn the_cache_returns_one_config_per_host() {
        let ca = ca();
        let first = config(&ca, "example.test", &[b"h2"]);
        let second = config(&ca, "example.test", &[b"h2"]);
        assert!(Arc::ptr_eq(&first, &second));
        let other = config(&ca, "example.test", &[b"http/1.1"]);
        assert!(
            !Arc::ptr_eq(&first, &other),
            "a different ALPN list rebuilds"
        );
        assert_eq!(other.alpn_protocols, vec![b"http/1.1".to_vec()]);
    }

    #[test]
    fn the_cache_is_bounded() {
        let ca = ca();
        for index in 0..=super::MAX_CACHED_LEAVES {
            config(&ca, &format!("host{index}.test"), &[b"h2"]);
        }
        let cache = ca.lock();
        assert_eq!(cache.configs.len(), super::MAX_CACHED_LEAVES);
        assert!(!cache.configs.contains_key("host0.test"), "the oldest went");
        assert!(cache.configs.contains_key("host1.test"));
    }
}

//! Endpoint TLS material.
//!
//! The kernel only ever holds *references* (`${secret:...}`) from a host
//! document; the bytes come from a [`SecretProvider`]. Both directions are
//! built here so an endpoint and the client that talks to it agree on what a
//! `tls` block means:
//!
//! * server: `cert` + `key`, optionally `client_ca` (mutual TLS);
//! * client: `ca`, optionally `client_cert` + `client_key`, optionally
//!   `server_name`.

use tonic::transport::{Certificate, ClientTlsConfig, Identity, ServerTlsConfig};

use crate::error::{Error, Result};

use crate::kernel::config::TlsConfig;
use crate::kernel::secret_provider::{SecretProvider, resolve};

/// Build the server side, or `None` when the block is empty.
pub fn server(config: &TlsConfig, secrets: &dyn SecretProvider) -> Result<Option<ServerTlsConfig>> {
    if is_empty(config) {
        return Ok(None);
    }
    let cert = resolve("tls.cert", &config.cert, secrets)?
        .ok_or_else(|| Error::validation("tls.cert is required when tls is configured"))?;
    let key = resolve("tls.key", &config.key, secrets)?
        .ok_or_else(|| Error::validation("tls.key is required when tls is configured"))?;
    let mut server = ServerTlsConfig::new().identity(Identity::from_pem(cert, key));
    if let Some(ca) = resolve("tls.client_ca", &config.client_ca, secrets)? {
        server = server.client_ca_root(Certificate::from_pem(ca));
    }
    Ok(Some(server))
}

/// Build the client side, or `None` when the block is empty.
pub fn client(config: &TlsConfig, secrets: &dyn SecretProvider) -> Result<Option<ClientTlsConfig>> {
    if is_empty(config) {
        return Ok(None);
    }
    let mut client = ClientTlsConfig::new();
    if let Some(ca) = resolve("tls.ca", &config.ca, secrets)? {
        client = client.ca_certificate(Certificate::from_pem(ca));
    }
    if let Some(cert) = resolve("tls.client_cert", &config.client_cert, secrets)?
        && let Some(key) = resolve("tls.client_key", &config.client_key, secrets)?
    {
        client = client.identity(Identity::from_pem(cert, key));
    }
    if !config.server_name.trim().is_empty() {
        client = client.domain_name(config.server_name.trim());
    }
    Ok(Some(client))
}

fn is_empty(config: &TlsConfig) -> bool {
    config.cert.is_empty()
        && config.key.is_empty()
        && config.client_ca.is_empty()
        && config.ca.is_empty()
        && config.client_cert.is_empty()
        && config.client_key.is_empty()
        && config.server_name.is_empty()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::kernel::secret_provider::EnvProvider;

    #[test]
    fn an_empty_block_means_no_tls() {
        let provider = EnvProvider::default();
        assert!(server(&TlsConfig::default(), &provider).unwrap().is_none());
        assert!(client(&TlsConfig::default(), &provider).unwrap().is_none());
    }

    #[test]
    fn server_requires_a_cert_and_key() {
        let provider = EnvProvider::default();
        let config = TlsConfig {
            cert: "${secret:missing}".to_owned(),
            ..TlsConfig::default()
        };
        assert!(server(&config, &provider).is_err());
    }

    #[test]
    fn inline_material_is_rejected_before_lookup() {
        let provider = EnvProvider::default();
        let config = TlsConfig {
            cert: "-----BEGIN CERTIFICATE-----".to_owned(),
            key: "${secret:key}".to_owned(),
            ..TlsConfig::default()
        };
        let error = server(&config, &provider).unwrap_err();
        assert!(error.to_string().contains("tls.cert"), "{error}");
    }
}

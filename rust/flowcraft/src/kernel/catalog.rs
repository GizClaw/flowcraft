//! The immutable endpoint catalog.

use std::collections::BTreeMap;

use crate::error::{Error, Result};

use crate::kernel::config::{Config, TlsConfig};
use crate::kernel::grant::Grant;

/// One resolved capability endpoint.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Endpoint {
    /// Logical identity a grant references.
    pub name: String,
    /// Transport address.
    pub target: String,
    /// The tenant it serves; empty means shared.
    pub tenant: String,
    /// Capabilities it serves; empty means "anything asked of it".
    pub capabilities: Vec<String>,
    /// TLS material, as secret references.
    pub tls: Option<TlsConfig>,
}

impl Endpoint {
    /// Whether this endpoint serves the capability.
    #[must_use]
    pub fn supports(&self, capability: &str) -> bool {
        if capability.is_empty() {
            return false;
        }
        self.capabilities.is_empty() || self.capabilities.iter().any(|item| item == capability)
    }

    /// Whether a grant may act on this endpoint with this capability.
    pub fn authorize(&self, grant: &Grant) -> Result<()> {
        if grant.endpoint != self.name {
            return Err(Error::forbidden(format!(
                "grant is for endpoint {:?}, not {:?}",
                grant.endpoint, self.name
            )));
        }
        if !self.tenant.is_empty() && grant.tenant != self.tenant {
            return Err(Error::forbidden(format!(
                "grant tenant {:?} does not match endpoint tenant {:?}",
                grant.tenant, self.tenant
            )));
        }
        if !self.supports(&grant.capability) {
            return Err(Error::not_available(format!(
                "endpoint {:?} does not serve capability {:?}",
                self.name, grant.capability
            )));
        }
        Ok(())
    }
}

/// A read-only catalog, built once from host configuration.
#[derive(Debug, Clone, Default)]
pub struct StaticCatalog {
    by_name: BTreeMap<String, Endpoint>,
}

impl StaticCatalog {
    /// Build from a validated configuration.
    #[must_use]
    pub fn from_config(config: &Config) -> Self {
        let by_name = config
            .remote
            .endpoints
            .iter()
            .map(|(name, endpoint)| {
                (
                    name.clone(),
                    Endpoint {
                        name: name.clone(),
                        target: endpoint.target.clone(),
                        tenant: endpoint.tenant.clone(),
                        capabilities: endpoint.capabilities.clone(),
                        tls: endpoint.tls.clone(),
                    },
                )
            })
            .collect();
        Self { by_name }
    }

    /// Resolve an endpoint by name.
    pub fn resolve(&self, name: &str) -> Result<Endpoint> {
        self.by_name
            .get(name)
            .cloned()
            .ok_or_else(|| Error::not_found(format!("endpoint {name:?} is not in the catalog")))
    }

    /// Every endpoint, in name order.
    #[must_use]
    pub fn list(&self) -> Vec<Endpoint> {
        self.by_name.values().cloned().collect()
    }

    /// How many endpoints are configured.
    #[must_use]
    pub fn len(&self) -> usize {
        self.by_name.len()
    }

    /// Whether the catalog is empty.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.by_name.is_empty()
    }
}

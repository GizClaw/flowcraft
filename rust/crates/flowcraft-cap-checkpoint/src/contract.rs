//! The checkpoint-store contract.

use std::sync::Arc;

use async_trait::async_trait;
use flowcraft_error::{Error, Result};
use flowcraft_resource::Resource;
use serde::Deserialize;

/// The registry kind.
pub const KIND: &str = "checkpoint.Store";
/// The cross-process contract identity.
pub const CONTRACT: &str = "checkpoint.Store/v1";

/// A durable, execution-keyed checkpoint store.
#[async_trait]
pub trait Store: Send + Sync {
    /// Store a checkpoint under `exec_id`, replacing any previous value.
    async fn save(&self, exec_id: &str, checkpoint: &[u8]) -> Result<()>;

    /// Load a checkpoint. `Ok(None)` means "no checkpoint for that execution".
    async fn load(&self, exec_id: &str) -> Result<Option<Vec<u8>>>;

    /// Every stored execution id, sorted.
    async fn list(&self) -> Result<Vec<String>>;

    /// Remove a checkpoint. Idempotent.
    async fn delete(&self, exec_id: &str) -> Result<()>;
}

/// The value a factory of this capability produces.
#[derive(Clone)]
pub struct Handle(pub Arc<dyn Store>);

impl Handle {
    /// Wrap a provider.
    #[must_use]
    pub fn new(store: Arc<dyn Store>) -> Self {
        Self(store)
    }

    /// The provider.
    #[must_use]
    pub fn store(&self) -> Arc<dyn Store> {
        self.0.clone()
    }
}

impl std::fmt::Debug for Handle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("checkpoint.Handle")
    }
}

impl std::ops::Deref for Handle {
    type Target = Arc<dyn Store>;

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl Resource for Handle {}

#[derive(Deserialize)]
struct Envelope {
    exec_id: String,
}

/// The execution id carried inside a checkpoint payload.
///
/// This is the only field the wire contract fixes: the Go client sends its
/// domain checkpoint, which happens to be JSON with `exec_id`, and the Go
/// endpoint reads exactly that. Rust reads the same field and keeps the bytes
/// opaque.
pub fn extract_exec_id(checkpoint: &[u8]) -> Result<String> {
    let envelope: Envelope = serde_json::from_slice(checkpoint).map_err(|error| {
        Error::validation(format!(
            "checkpoint payload must be JSON with an exec_id field: {error}"
        ))
    })?;
    if envelope.exec_id.trim().is_empty() {
        return Err(Error::validation("checkpoint payload has an empty exec_id"));
    }
    Ok(envelope.exec_id)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_the_id_and_ignores_the_rest() {
        let payload = br#"{"exec_id":"run-1","board":{"messages":[]},"extra":true}"#;
        assert_eq!(extract_exec_id(payload).unwrap(), "run-1");
    }

    #[test]
    fn rejects_a_payload_without_an_id() {
        let error = extract_exec_id(br#"{"board":{}}"#).unwrap_err();
        assert!(error.to_string().contains("exec_id"), "{error}");
    }

    #[test]
    fn rejects_an_empty_id() {
        assert!(extract_exec_id(br#"{"exec_id":" "}"#).is_err());
    }

    #[test]
    fn rejects_non_json() {
        assert!(extract_exec_id(b"not json").is_err());
    }
}

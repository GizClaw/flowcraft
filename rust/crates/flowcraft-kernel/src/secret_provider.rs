//! Bootstrap secrets: the material the kernel needs before any deployment
//! resource exists (endpoint TLS keys).
//!
//! This is deliberately *not* the application `secret.Store`: that one is a
//! capability resource with a grant-authenticated endpoint, and pointing the
//! kernel at it would be circular. Bootstrap secrets come from the host:
//! environment variables or files, in order.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use flowcraft_error::{Error, Result};

/// How large one bootstrap secret file may be. Bootstrap material is a key or a
/// certificate, never a document.
pub const MAX_BYTES: u64 = 1 << 20;

/// Reject a name that cannot be a reference: empty, or carrying whitespace.
///
/// Slashes and dots are fine — `${secret:remote/ca}` and
/// `${secret:client-cert.pem}` are the conventional spellings.
fn validate_name(name: &str) -> Result<()> {
    if name.trim().is_empty() {
        return Err(Error::validation("secret name is empty"));
    }
    if name != name.trim() || name.contains([' ', '\t', '\r', '\n', '\0', '=']) {
        return Err(Error::validation(format!(
            "secret name {name:?} contains whitespace or an invalid character"
        )));
    }
    Ok(())
}

/// A host-side secret source, returning raw bytes (PEM, keys).
pub trait SecretProvider: Send + Sync + std::fmt::Debug {
    /// Resolve a name. `Ok(None)` means "not here"; try the next source.
    fn lookup(&self, name: &str) -> Result<Option<Vec<u8>>>;
}

/// Reads secret names from the process environment.
#[derive(Debug, Default)]
pub struct EnvProvider {
    prefix: String,
}

impl EnvProvider {
    /// Read `prefix + name` from the environment.
    #[must_use]
    pub fn new(prefix: impl Into<String>) -> Self {
        Self {
            prefix: prefix.into(),
        }
    }
}

impl SecretProvider for EnvProvider {
    fn lookup(&self, name: &str) -> Result<Option<Vec<u8>>> {
        validate_name(name)?;
        Ok(std::env::var(format!("{}{name}", self.prefix))
            .ok()
            .map(String::into_bytes))
    }
}

/// Reads secret names from files under a directory.
#[derive(Debug)]
pub struct FileProvider {
    root: PathBuf,
}

impl FileProvider {
    /// Read `root/name`.
    ///
    /// The directory must already exist: a missing bootstrap directory is a
    /// deployment mistake, not something to paper over by creating an empty one
    /// (which would then fail every lookup).
    pub fn new(root: impl Into<PathBuf>) -> Result<Self> {
        let root = root.into();
        let metadata = std::fs::metadata(&root).map_err(|error| {
            Error::validation(format!(
                "bootstrap secrets: dir {} is not usable: {error}",
                root.display()
            ))
        })?;
        if !metadata.is_dir() {
            return Err(Error::validation(format!(
                "bootstrap secrets: dir {} is not a directory",
                root.display()
            )));
        }
        Ok(Self {
            root: root.canonicalize()?,
        })
    }

    fn path(&self, name: &str) -> Result<PathBuf> {
        validate_name(name)?;
        let relative = Path::new(name);
        if relative.is_absolute()
            || relative
                .components()
                .any(|component| !matches!(component, std::path::Component::Normal(_)))
        {
            return Err(Error::validation(format!(
                "secret name {name:?} is not a safe file name"
            )));
        }
        Ok(self.root.join(relative))
    }
}

impl SecretProvider for FileProvider {
    fn lookup(&self, name: &str) -> Result<Option<Vec<u8>>> {
        let path = self.path(name)?;
        let metadata = match std::fs::symlink_metadata(&path) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(error) => {
                return Err(Error::not_found(format!(
                    "secret file {}: {error}",
                    path.display()
                )));
            }
        };
        if metadata.file_type().is_symlink() {
            return Err(Error::validation(format!(
                "secret file {}: a bootstrap secret must not be a symlink",
                path.display()
            )));
        }
        if !metadata.is_file() {
            return Err(Error::validation(format!(
                "secret file {}: not a regular file",
                path.display()
            )));
        }
        if metadata.len() > MAX_BYTES {
            return Err(Error::validation(format!(
                "secret file {}: {} bytes is over the {MAX_BYTES}-byte limit",
                path.display(),
                metadata.len()
            )));
        }
        match std::fs::read(&path) {
            Ok(bytes) => Ok(Some(bytes)),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(Error::not_found(format!(
                "secret file {}: {error}",
                path.display()
            ))),
        }
    }
}

/// Tries providers in order; the first hit wins.
#[derive(Debug, Default)]
pub struct Chain {
    providers: Vec<Arc<dyn SecretProvider>>,
}

impl Chain {
    /// Build from providers, in order.
    #[must_use]
    pub fn new(providers: Vec<Arc<dyn SecretProvider>>) -> Self {
        Self { providers }
    }
}

impl SecretProvider for Chain {
    fn lookup(&self, name: &str) -> Result<Option<Vec<u8>>> {
        // Validate at the boundary too: an empty chain has no provider left to
        // reject a name that could never be a reference.
        validate_name(name)?;
        for provider in &self.providers {
            if let Some(value) = provider.lookup(name)? {
                return Ok(Some(value));
            }
        }
        Ok(None)
    }
}

/// The `name` inside a `${secret:name}` reference.
pub fn reference_name(field: &str, value: &str) -> Result<Option<String>> {
    let value = value.trim();
    if value.is_empty() {
        return Ok(None);
    }
    if let Some(rest) = value.strip_prefix("${secret:") {
        let name = rest
            .strip_suffix('}')
            .ok_or_else(|| Error::validation(format!("{field}: unterminated secret reference")))?;
        if name.is_empty() {
            return Err(Error::validation(format!(
                "{field}: empty secret reference"
            )));
        }
        return Ok(Some(name.to_owned()));
    }
    Err(Error::validation(format!(
        "{field}: {value:?} must be a ${{secret:...}} reference, not inline material"
    )))
}

/// Resolve one reference against a provider.
pub fn resolve(field: &str, value: &str, secrets: &dyn SecretProvider) -> Result<Option<Vec<u8>>> {
    let Some(name) = reference_name(field, value)? else {
        return Ok(None);
    };
    secrets.lookup(&name)?.map(Some).ok_or_else(|| {
        Error::not_found(format!(
            "{field}: bootstrap secret {name:?} is not available"
        ))
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn references_are_parsed_strictly() {
        assert_eq!(
            reference_name("tls.cert", "${secret:remote/ca}").unwrap(),
            Some("remote/ca".to_owned())
        );
        assert_eq!(reference_name("tls.cert", "").unwrap(), None);
        assert!(reference_name("tls.cert", "-----BEGIN").is_err());
        assert!(reference_name("tls.cert", "${secret:}").is_err());
        assert!(reference_name("tls.cert", "${secret:x").is_err());
    }

    #[test]
    fn the_chain_takes_the_first_hit() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("present"), b"value").unwrap();
        let provider = Chain::new(vec![
            Arc::new(FileProvider::new(dir.path()).unwrap()),
            Arc::new(EnvProvider::new("FLOWCRAFT_TEST_")),
        ]);
        assert_eq!(provider.lookup("present").unwrap(), Some(b"value".to_vec()));
        assert_eq!(provider.lookup("absent").unwrap(), None);
    }

    #[test]
    fn files_are_served_and_escapes_rejected() {
        let dir = tempfile::tempdir().unwrap();
        let provider = FileProvider::new(dir.path()).unwrap();
        std::fs::write(dir.path().join("ca.pem"), b"pem").unwrap();
        assert_eq!(provider.lookup("ca.pem").unwrap(), Some(b"pem".to_vec()));
        assert_eq!(provider.lookup("missing.pem").unwrap(), None);
        assert!(provider.lookup("../escape").is_err());
    }

    #[test]
    fn a_directory_that_does_not_exist_is_a_mistake() {
        let dir = tempfile::tempdir().unwrap();
        let missing = dir.path().join("nope");
        let error = FileProvider::new(&missing).unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
        assert!(error.to_string().contains("not usable"), "{error}");
    }

    #[test]
    fn a_symlinked_bootstrap_secret_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        std::fs::write(outside.path().join("pem"), b"pem").unwrap();
        #[cfg(unix)]
        {
            std::os::unix::fs::symlink(outside.path().join("pem"), dir.path().join("link.pem"))
                .unwrap();
            let provider = FileProvider::new(dir.path()).unwrap();
            let error = provider.lookup("link.pem").unwrap_err();
            assert!(error.to_string().contains("symlink"), "{error}");
        }
    }

    #[test]
    fn an_oversized_bootstrap_secret_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(
            dir.path().join("huge.pem"),
            vec![b'x'; MAX_BYTES as usize + 1],
        )
        .unwrap();
        let provider = FileProvider::new(dir.path()).unwrap();
        let error = provider.lookup("huge.pem").unwrap_err();
        assert!(error.to_string().contains("over the"), "{error}");
    }

    #[test]
    fn names_with_whitespace_are_refused() {
        let provider = Chain::default();
        assert!(provider.lookup("a b").is_err());
        assert!(provider.lookup("").is_err());
        // Conventional spellings still work.
        assert!(provider.lookup("remote/ca").is_ok());
        assert!(provider.lookup("client-cert.pem").is_ok());
    }

    #[test]
    fn a_missing_reference_names_the_field() {
        let provider = Chain::default();
        let error = resolve("tls.key", "${secret:nope}", &provider).unwrap_err();
        assert!(error.to_string().contains("tls.key"), "{error}");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotFound);
    }
}

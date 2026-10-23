//! Transport: how a client reaches an endpoint target.

use std::path::Path;

use flowcraft_error::{Error, Result};
use tonic::transport::{Channel, ClientTlsConfig, Endpoint};

use crate::config::Target;

/// Open a channel to `target`, applying TLS when the endpoint has it.
pub async fn connect(target: &Target, tls: Option<ClientTlsConfig>) -> Result<Channel> {
    match target {
        Target::Unix(path) => {
            if tls.is_some() {
                return Err(Error::validation("a unix target cannot carry tls material"));
            }
            connect_unix(path).await
        }
        Target::Dns(address) => {
            let mut endpoint = Endpoint::try_from(format!("https://{address}"))
                .map_err(|error| Error::validation(format!("endpoint {address:?}: {error}")))?;
            if let Some(tls) = tls {
                endpoint = endpoint.tls_config(tls).map_err(|error| {
                    Error::validation(format!("endpoint {address:?} tls: {error}"))
                })?;
            }
            endpoint.connect().await.map_err(|error| {
                Error::not_available(format!("cannot reach endpoint {address:?}: {error}"))
            })
        }
    }
}

/// Open a channel over a unix-domain socket.
pub async fn connect_unix(path: &Path) -> Result<Channel> {
    let path = path.to_path_buf();
    let endpoint = Endpoint::try_from("http://flowcraft.local")
        .map_err(|error| Error::internal(format!("client endpoint: {error}")))?;
    endpoint
        .connect_with_connector(tower::service_fn(move |_: http::Uri| {
            let path = path.clone();
            async move {
                let stream = tokio::net::UnixStream::connect(path).await?;
                Ok::<_, std::io::Error>(hyper_util::rt::TokioIo::new(stream))
            }
        }))
        .await
        .map_err(|error| Error::not_available(format!("cannot reach endpoint: {error}")))
}

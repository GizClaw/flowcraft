//! The craft ABI contract, enforced by the platform.
//!
//! The lifecycle RPCs are platform-owned: a craft must implement them exactly.
//! Business payloads are typed: a run or a steer carries an [`pb::Envelope`]
//! whose body is either a `flowcraft.message.v1` message, a streaming delta, or
//! a craft-private [`pb::Extension`]. That is what keeps the conversation shape
//! in one place (`proto/flowcraft/message/v1/message.proto`) instead of being a
//! convention each craft re-invents, while still letting a craft carry its own
//! payloads opaquely.
//!
//! Capability names are namespaced too: `provides` may only advertise private
//! capabilities (`<vendor>.<craft>.<thing>/vN`), never a platform capability.

use flowcraft::error::{Error, Result};
use flowcraft::proto::craft as pb;
use flowcraft::proto::message as msg;

/// The lifecycle ABI this platform implements.
pub const ABI_VERSION: &str = "flowcraft.craft/v1";

/// The platform-level event kinds a `RunEvent` may carry.
pub const EVENT_TYPES: &[&str] = &["delta", "message", "credit", "done", "error"];

/// Prefixes reserved for platform capabilities.
const PLATFORM_PREFIXES: &[&str] = &[
    "workspace.",
    "sandbox.",
    "secret.",
    "checkpoint.",
    "inference.",
    "tool.",
    "memory.",
    "event.",
    "graph.",
    "agent.",
    "craft.",
];

/// Check a craft's self-description before the platform opens a session.
pub fn validate_info(info: &pb::CraftInfo) -> Result<()> {
    if info.name.trim().is_empty() {
        return Err(Error::validation("craft: Describe returned an empty name"));
    }
    let supported: Vec<&str> = if info.abi_versions.is_empty() {
        vec![info.abi_version.as_str()]
    } else {
        info.abi_versions.iter().map(String::as_str).collect()
    };
    if !supported.contains(&ABI_VERSION) {
        return Err(Error::not_available(format!(
            "craft {:?} implements {supported:?}; this platform supports {ABI_VERSION}",
            info.name
        )));
    }
    // The declared lists name the *extension* types the craft accepts or emits.
    // `message` and `delta` are platform vocabulary and need no declaration.
    for ty in info.input_types.iter().chain(info.event_types.iter()) {
        validate_type_name(ty)
            .map_err(|error| Error::new(error.kind(), format!("craft {:?}: {error}", info.name)))?;
        if is_platform_payload_type(ty) {
            return Err(Error::validation(format!(
                "craft {:?}: {ty:?} is platform vocabulary and must not be declared as an extension",
                info.name
            )));
        }
    }
    for capability in &info.provides {
        validate_private_capability(capability)
            .map_err(|error| Error::new(error.kind(), format!("craft {:?}: {error}", info.name)))?;
    }
    Ok(())
}

fn is_platform_payload_type(name: &str) -> bool {
    matches!(name, "message" | "delta")
}

/// Envelope type names are lowercase dotted names.
fn validate_type_name(name: &str) -> Result<()> {
    if name.is_empty() {
        return Err(Error::validation("envelope type is empty"));
    }
    let body = name.strip_prefix("x-").unwrap_or(name);
    if body.is_empty()
        || !body
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, '.' | '_' | '-'))
    {
        return Err(Error::validation(format!(
            "envelope type {name:?} must be a lowercase dotted name"
        )));
    }
    Ok(())
}

/// A craft may only advertise private capabilities, never a platform one.
pub fn validate_private_capability(capability: &str) -> Result<()> {
    let Some((name, version)) = capability.rsplit_once('/') else {
        return Err(Error::validation(format!(
            "capability {capability:?} must be <name>/v<N>"
        )));
    };
    let Some(number) = version.strip_prefix('v') else {
        return Err(Error::validation(format!(
            "capability {capability:?} must end in /v<N>"
        )));
    };
    if number.is_empty() || !number.chars().all(|c| c.is_ascii_digit()) {
        return Err(Error::validation(format!(
            "capability {capability:?} must end in /v<N>"
        )));
    }
    if PLATFORM_PREFIXES
        .iter()
        .any(|prefix| name.starts_with(prefix))
    {
        return Err(Error::forbidden(format!(
            "capability {capability:?} uses a platform namespace; a craft may only provide <vendor>.<craft>.<thing>/v<N>"
        )));
    }
    if name.split('.').count() < 3 {
        return Err(Error::validation(format!(
            "capability {capability:?} must be namespaced as <vendor>.<craft>.<thing>/v<N>"
        )));
    }
    Ok(())
}

/// Check a `RunRequest.input` / `SteerRequest.payload` before sending it.
pub fn validate_input(info: &pb::CraftInfo, envelope: &pb::Envelope) -> Result<()> {
    match envelope.body.as_ref() {
        Some(pb::envelope::Body::Message(message)) => flowcraft::message::validate_message(message)
            .map_err(|error| Error::new(error.kind(), format!("input message: {error}"))),
        Some(pb::envelope::Body::Delta(delta)) => flowcraft::message::validate_delta(delta)
            .map_err(|error| Error::new(error.kind(), format!("input delta: {error}"))),
        Some(pb::envelope::Body::Extension(extension)) => {
            validate_extension(extension, &info.input_types, "input")
        }
        None => Err(Error::validation(
            "input envelope carries no body (message, delta, or extension)",
        )),
    }
}

/// Check a `RunEvent` while consuming the stream.
pub fn validate_event(info: &pb::CraftInfo, event: &pb::RunEvent) -> Result<()> {
    if !EVENT_TYPES.contains(&event.r#type.as_str()) {
        return Err(Error::validation(format!(
            "craft {:?} emitted unknown event type {:?}",
            info.name, event.r#type
        )));
    }
    match event.r#type.as_str() {
        "delta" => match event
            .payload
            .as_ref()
            .and_then(|envelope| envelope.body.as_ref())
        {
            Some(pb::envelope::Body::Delta(delta)) => flowcraft::message::validate_delta(delta)
                .map_err(|error| Error::new(error.kind(), format!("event delta: {error}"))),
            Some(pb::envelope::Body::Extension(extension)) => {
                validate_extension(extension, &info.event_types, "event")
            }
            Some(pb::envelope::Body::Message(_)) => Err(Error::validation(
                "a delta event must carry a Delta payload, not a Message",
            )),
            None => Err(Error::validation("a delta event carries no payload")),
        },
        "message" => match event
            .payload
            .as_ref()
            .and_then(|envelope| envelope.body.as_ref())
        {
            Some(pb::envelope::Body::Message(message)) => {
                flowcraft::message::validate_message(message)
                    .map_err(|error| Error::new(error.kind(), format!("event message: {error}")))
            }
            Some(pb::envelope::Body::Extension(extension)) => {
                validate_extension(extension, &info.event_types, "event")
            }
            Some(pb::envelope::Body::Delta(_)) => Err(Error::validation(
                "a message event must carry a Message payload, not a Delta",
            )),
            None => Err(Error::validation("a message event carries no payload")),
        },
        "error" if event.error.trim().is_empty() => Err(Error::validation(
            "craft emitted an error event with no message",
        )),
        _ => Ok(()),
    }
}

fn validate_extension(extension: &pb::Extension, declared: &[String], what: &str) -> Result<()> {
    validate_type_name(&extension.r#type)?;
    if is_platform_payload_type(&extension.r#type) {
        return Err(Error::validation(format!(
            "{what} extension type {:?} is platform vocabulary; use the typed body",
            extension.r#type
        )));
    }
    if extension.r#type.starts_with("x-") {
        return Ok(());
    }
    if !declared
        .iter()
        .any(|declared| declared == &extension.r#type)
    {
        return Err(Error::validation(format!(
            "{what} extension type {:?} is not declared by the craft",
            extension.r#type
        )));
    }
    Ok(())
}

/// Build an envelope carrying one text message.
#[must_use]
pub fn text_message(role: msg::Role, text: &str) -> pb::Envelope {
    pb::Envelope {
        body: Some(pb::envelope::Body::Message(
            flowcraft::message::text_message(role, text),
        )),
    }
}

/// Build an envelope carrying one text delta.
#[must_use]
pub fn delta_text(text: &str) -> pb::Envelope {
    pb::Envelope {
        body: Some(pb::envelope::Body::Delta(msg::Delta {
            kind: Some(msg::delta::Kind::Text(text.to_owned())),
        })),
    }
}

/// Build an envelope carrying a craft-private extension.
#[must_use]
pub fn extension(kind: &str, data: Vec<u8>) -> pb::Envelope {
    pb::Envelope {
        body: Some(pb::envelope::Body::Extension(pb::Extension {
            r#type: kind.to_owned(),
            data,
        })),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn info() -> pb::CraftInfo {
        pb::CraftInfo {
            name: "demo".to_owned(),
            version: "1".to_owned(),
            requires: Vec::new(),
            provides: vec!["acme.demo.tools/v1".to_owned()],
            tenancy: "single".to_owned(),
            abi_version: ABI_VERSION.to_owned(),
            abi_versions: Vec::new(),
            input_types: vec!["stub.text".to_owned()],
            event_types: vec!["stub.text".to_owned()],
        }
    }

    #[test]
    fn a_valid_info_passes() {
        validate_info(&info()).expect("valid");
    }

    #[test]
    fn an_incompatible_abi_is_refused() {
        let mut info = info();
        info.abi_version = "flowcraft.craft/v9".to_owned();
        assert_eq!(
            validate_info(&info).unwrap_err().kind(),
            flowcraft::error::ErrorKind::NotAvailable
        );
    }

    #[test]
    fn a_platform_capability_cannot_be_provided() {
        let mut info = info();
        info.provides = vec!["workspace.Workspace/v1".to_owned()];
        assert_eq!(
            validate_info(&info).unwrap_err().kind(),
            flowcraft::error::ErrorKind::Forbidden
        );
    }

    #[test]
    fn provides_need_a_private_namespace() {
        let mut info = info();
        info.provides = vec!["tools/v1".to_owned()];
        assert!(validate_info(&info).is_err());
    }

    #[test]
    fn platform_payload_types_cannot_be_declared_as_extensions() {
        let mut info = info();
        info.input_types = vec!["message".to_owned()];
        assert!(validate_info(&info).is_err());
    }

    #[test]
    fn typed_input_is_validated() {
        let info = info();
        validate_input(&info, &text_message(msg::Role::User, "hello")).expect("message");
        validate_input(&info, &delta_text("hel")).expect("delta");
        validate_input(&info, &extension("stub.text", vec![1])).expect("declared extension");
        validate_input(&info, &extension("x-private", vec![1])).expect("x- extension");
        assert!(validate_input(&info, &extension("not.declared", vec![])).is_err());
        assert!(validate_input(&info, &pb::Envelope { body: None }).is_err());
    }

    #[test]
    fn a_message_needs_a_role_and_a_part() {
        let info = info();
        let mut envelope = text_message(msg::Role::User, "hello");
        if let Some(pb::envelope::Body::Message(message)) = envelope.body.as_mut() {
            message.role = msg::Role::Unspecified as i32;
        }
        assert!(validate_input(&info, &envelope).is_err());

        let empty = pb::Envelope {
            body: Some(pb::envelope::Body::Message(msg::Message {
                role: msg::Role::Assistant as i32,
                content: Some(msg::Content { parts: Vec::new() }),
            })),
        };
        assert!(validate_input(&info, &empty).is_err());
    }

    #[test]
    fn events_are_checked_per_kind() {
        let info = info();
        let delta = pb::RunEvent {
            seq: 1,
            r#type: "delta".to_owned(),
            payload: Some(delta_text("hi")),
            error: String::new(),
            credit: 0,
        };
        validate_event(&info, &delta).expect("delta");

        let wrong = pb::RunEvent {
            r#type: "delta".to_owned(),
            payload: Some(text_message(msg::Role::Assistant, "hi")),
            ..delta.clone()
        };
        assert!(validate_event(&info, &wrong).is_err());

        let done = pb::RunEvent {
            r#type: "done".to_owned(),
            payload: None,
            ..delta.clone()
        };
        assert!(validate_event(&info, &done).is_ok());

        let unknown = pb::RunEvent {
            r#type: "surprise".to_owned(),
            ..delta.clone()
        };
        assert!(validate_event(&info, &unknown).is_err());

        let empty_error = pb::RunEvent {
            r#type: "error".to_owned(),
            payload: None,
            error: String::new(),
            ..delta
        };
        assert!(validate_event(&info, &empty_error).is_err());
    }
}

//! The one error type every call returns.

use std::fmt;
use std::time::Duration;

/// Where every error code is documented.
pub const DOCS_ERRORS_URL: &str = "https://zipbox.ai/docs/errors";

/// The public error code. The code is the contract; the message is for humans.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum ErrorCode {
    /// The request was malformed (400, 413, 422).
    InvalidRequest,
    /// The credential is missing, wrong or deleted (401).
    Unauthorized,
    /// The credential cannot do this (403), e.g. an API key minting keys.
    Forbidden,
    /// No such box, command or file (404).
    NotFound,
    /// The account is out of credit (402).
    InsufficientCredits,
    /// The request conflicts with the current state (409).
    Conflict,
    /// Too many requests (429). Retried automatically on safe requests.
    RateLimited,
    /// The box is not running or not reachable yet.
    BoxNotRunning,
    /// The service is temporarily unavailable (5xx, network failure).
    Unavailable,
}

impl ErrorCode {
    /// The wire name, e.g. `not_found`.
    pub fn as_str(self) -> &'static str {
        match self {
            ErrorCode::InvalidRequest => "invalid_request",
            ErrorCode::Unauthorized => "unauthorized",
            ErrorCode::Forbidden => "forbidden",
            ErrorCode::NotFound => "not_found",
            ErrorCode::InsufficientCredits => "insufficient_credits",
            ErrorCode::Conflict => "conflict",
            ErrorCode::RateLimited => "rate_limited",
            ErrorCode::BoxNotRunning => "box_not_running",
            ErrorCode::Unavailable => "unavailable",
        }
    }

    /// Parses a wire name. Unknown names classify as [`ErrorCode::Unavailable`].
    pub fn parse(value: &str) -> ErrorCode {
        match value {
            "invalid_request" => ErrorCode::InvalidRequest,
            "unauthorized" => ErrorCode::Unauthorized,
            "forbidden" => ErrorCode::Forbidden,
            "not_found" => ErrorCode::NotFound,
            "insufficient_credits" => ErrorCode::InsufficientCredits,
            "conflict" => ErrorCode::Conflict,
            "rate_limited" => ErrorCode::RateLimited,
            "box_not_running" => ErrorCode::BoxNotRunning,
            _ => ErrorCode::Unavailable,
        }
    }

    /// The code for an HTTP status that carried no error envelope.
    pub fn for_status(status: u16) -> ErrorCode {
        match status {
            400 | 413 | 422 => ErrorCode::InvalidRequest,
            401 => ErrorCode::Unauthorized,
            402 => ErrorCode::InsufficientCredits,
            403 => ErrorCode::Forbidden,
            404 => ErrorCode::NotFound,
            409 => ErrorCode::Conflict,
            429 => ErrorCode::RateLimited,
            502 => ErrorCode::BoxNotRunning,
            s if s >= 500 => ErrorCode::Unavailable,
            _ => ErrorCode::InvalidRequest,
        }
    }

    /// Whether the same request may succeed later.
    pub fn is_retryable(self) -> bool {
        matches!(
            self,
            ErrorCode::RateLimited | ErrorCode::Unavailable | ErrorCode::BoxNotRunning
        )
    }
}

impl fmt::Display for ErrorCode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A failed call. A command that exits non-zero is a result, never an `Error`.
#[derive(Debug, thiserror::Error)]
#[error("{code}: {message}")]
pub struct Error {
    /// The public error code.
    pub code: ErrorCode,
    /// The HTTP status, when the failure came from a response.
    pub status: Option<u16>,
    /// Human-readable detail.
    pub message: String,
    /// The server's request id, when it sent one.
    pub request_id: Option<String>,
    /// Whether the same request may succeed later.
    pub retryable: bool,
    /// How long the server asked the caller to wait.
    pub retry_after: Option<Duration>,
    /// `https://zipbox.ai/docs/errors#<code>`.
    pub docs_url: String,
    /// The underlying transport or decoding error, if any.
    #[source]
    pub source: Option<Box<dyn std::error::Error + Send + Sync>>,
}

impl Error {
    /// An error with a code and message and nothing else.
    pub fn new(code: ErrorCode, message: impl Into<String>) -> Error {
        Error {
            code,
            status: None,
            message: message.into(),
            request_id: None,
            retryable: code.is_retryable(),
            retry_after: None,
            docs_url: docs_url(code),
            source: None,
        }
    }

    pub(crate) fn transport(err: impl std::error::Error + Send + Sync + 'static) -> Error {
        let mut e = Error::new(ErrorCode::Unavailable, err.to_string());
        e.source = Some(Box::new(err));
        e
    }

    pub(crate) fn decode(err: impl std::error::Error + Send + Sync + 'static) -> Error {
        let mut e = Error::new(
            ErrorCode::Unavailable,
            format!("unexpected response: {err}"),
        );
        e.retryable = false;
        e.source = Some(Box::new(err));
        e
    }

    /// Builds the error for a non-2xx response body.
    pub(crate) fn from_response(status: u16, retry_after: Option<Duration>, body: &[u8]) -> Error {
        let parsed: Option<serde_json::Value> = serde_json::from_slice(body).ok();
        if let Some(envelope) = parsed
            .as_ref()
            .and_then(|v| v.get("error"))
            .filter(|e| e.is_object())
        {
            let code = envelope
                .get("code")
                .and_then(|c| c.as_str())
                .map(ErrorCode::parse)
                .unwrap_or_else(|| ErrorCode::for_status(status));
            let retry_after = envelope
                .get("retryAfterMs")
                .and_then(|v| v.as_u64())
                .map(Duration::from_millis)
                .or(retry_after);
            return Error {
                code,
                status: Some(status),
                message: envelope
                    .get("message")
                    .and_then(|m| m.as_str())
                    .unwrap_or("")
                    .to_string(),
                request_id: envelope
                    .get("requestId")
                    .and_then(|r| r.as_str())
                    .map(str::to_string),
                retryable: envelope
                    .get("retryable")
                    .and_then(|r| r.as_bool())
                    .unwrap_or_else(|| code.is_retryable()),
                retry_after,
                docs_url: envelope
                    .get("docsUrl")
                    .and_then(|d| d.as_str())
                    .map(str::to_string)
                    .unwrap_or_else(|| docs_url(code)),
                source: None,
            };
        }
        let code = ErrorCode::for_status(status);
        let message = parsed
            .as_ref()
            .and_then(|v| v.get("error"))
            .and_then(|e| e.as_str())
            .map(str::to_string)
            .unwrap_or_else(|| String::from_utf8_lossy(body).trim().to_string());
        Error {
            code,
            status: Some(status),
            message,
            request_id: None,
            retryable: code.is_retryable(),
            retry_after,
            docs_url: docs_url(code),
            source: None,
        }
    }
}

fn docs_url(code: ErrorCode) -> String {
    format!("{DOCS_ERRORS_URL}#{}", code.as_str())
}

/// `Result<T, zipbox::Error>`.
pub type Result<T> = std::result::Result<T, Error>;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn maps_statuses() {
        assert_eq!(ErrorCode::for_status(413), ErrorCode::InvalidRequest);
        assert_eq!(ErrorCode::for_status(402), ErrorCode::InsufficientCredits);
        assert_eq!(ErrorCode::for_status(502), ErrorCode::BoxNotRunning);
        assert_eq!(ErrorCode::for_status(503), ErrorCode::Unavailable);
    }

    #[test]
    fn parses_envelope_and_legacy_bodies() {
        let e = Error::from_response(
            429,
            None,
            br#"{"error":{"code":"rate_limited","message":"slow","requestId":"r","retryable":true,"retryAfterMs":1500,"docsUrl":"https://zipbox.ai/docs/errors#rate_limited"}}"#,
        );
        assert_eq!(e.code, ErrorCode::RateLimited);
        assert_eq!(e.retry_after, Some(Duration::from_millis(1500)));
        assert_eq!(e.request_id.as_deref(), Some("r"));
        let legacy = Error::from_response(404, None, br#"{"error":"no such file"}"#);
        assert_eq!(legacy.code, ErrorCode::NotFound);
        assert_eq!(legacy.message, "no such file");
        assert_eq!(legacy.docs_url, "https://zipbox.ai/docs/errors#not_found");
    }
}

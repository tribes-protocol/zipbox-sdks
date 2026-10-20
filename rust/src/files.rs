//! Files on a box.

use std::path::Path;

use reqwest::Method;

use crate::client::{encode, json_body, Body};
use crate::error::{Error, ErrorCode, Result};
use crate::sandbox::Sandbox;
use crate::types::{FileEntry, FileListing};

/// Relative paths resolve against this directory.
pub const WORKSPACE_DIR: &str = "/root/workspace";

/// The largest file a single read or write moves (250 MiB).
pub const MAX_FILE_BYTES: u64 = 250 * 1024 * 1024;

/// Files on a box, from [`Sandbox::files`]. Paths are absolute or relative to
/// `/root/workspace`.
#[derive(Debug, Clone)]
pub struct Files {
    pub(crate) sandbox: Sandbox,
}

impl Files {
    /// A file's bytes (up to 250 MiB).
    pub async fn read(&self, path: &str) -> Result<Vec<u8>> {
        let path = resolve_path(path);
        let resp = self
            .sandbox
            .client
            .box_request(
                self.sandbox.id(),
                Method::GET,
                &format!("/files/download?path={}", encode(&path)),
                || Body::None,
                None,
            )
            .await?;
        Ok(resp.bytes().await.map_err(Error::transport)?.to_vec())
    }

    /// A file as text (lossy UTF-8).
    pub async fn read_text(&self, path: &str) -> Result<String> {
        Ok(String::from_utf8_lossy(&self.read(path).await?).into_owned())
    }

    /// Writes a file atomically (up to 250 MiB). The parent directory must exist.
    pub async fn write(&self, path: &str, content: impl Into<Vec<u8>>) -> Result<()> {
        let path = resolve_path(path);
        let (dir, name) = split(&path)?;
        let content = content.into();
        self.sandbox
            .client
            .box_request(
                self.sandbox.id(),
                Method::POST,
                &format!("/files/write?dir={}&name={}", encode(dir), encode(name)),
                || Body::Bytes(content.clone()),
                None,
            )
            .await?;
        Ok(())
    }

    /// A directory's entries.
    pub async fn list(&self, path: &str) -> Result<Vec<FileEntry>> {
        let path = resolve_path(path);
        let listing: FileListing = self
            .sandbox
            .client
            .box_json(
                self.sandbox.id(),
                Method::GET,
                &format!("/files?path={}", encode(&path)),
                || Body::None,
            )
            .await?;
        Ok(listing.entries)
    }

    /// One entry, read from its parent directory's listing.
    pub async fn stat(&self, path: &str) -> Result<FileEntry> {
        let path = resolve_path(path);
        let (dir, name) = split(&path)?;
        self.list(dir)
            .await?
            .into_iter()
            .find(|entry| entry.name == name)
            .ok_or_else(|| Error::new(ErrorCode::NotFound, format!("no such file: {path}")))
    }

    /// Creates a directory.
    pub async fn mkdir(&self, path: &str) -> Result<()> {
        self.mutate(serde_json::json!({ "action": "mkdir", "path": resolve_path(path) }))
            .await
    }

    /// Moves or renames `path` to `destination`.
    pub async fn move_to(&self, path: &str, destination: &str) -> Result<()> {
        self.mutate(serde_json::json!({
            "action": "move",
            "path": resolve_path(path),
            "destination": resolve_path(destination),
        }))
        .await
    }

    /// Deletes a file or directory.
    pub async fn delete(&self, path: &str) -> Result<()> {
        self.mutate(serde_json::json!({ "action": "delete", "path": resolve_path(path) }))
            .await
    }

    /// Uploads a local file to `remote_path`.
    pub async fn upload(&self, local_path: impl AsRef<Path>, remote_path: &str) -> Result<()> {
        let bytes = tokio::fs::read(local_path).await.map_err(|err| {
            Error::new(
                ErrorCode::InvalidRequest,
                format!("cannot read local file: {err}"),
            )
        })?;
        self.write(remote_path, bytes).await
    }

    /// Downloads `remote_path` to a local file.
    pub async fn download(&self, remote_path: &str, local_path: impl AsRef<Path>) -> Result<()> {
        let bytes = self.read(remote_path).await?;
        tokio::fs::write(local_path, bytes).await.map_err(|err| {
            Error::new(
                ErrorCode::InvalidRequest,
                format!("cannot write local file: {err}"),
            )
        })
    }

    async fn mutate(&self, body: serde_json::Value) -> Result<()> {
        self.sandbox
            .client
            .box_request(
                self.sandbox.id(),
                Method::POST,
                "/files/mutate",
                || json_body(&body),
                None,
            )
            .await?;
        Ok(())
    }
}

/// Joins a relative path to `/root/workspace`.
pub(crate) fn resolve_path(path: &str) -> String {
    if path.starts_with('/') {
        path.to_string()
    } else {
        format!("{WORKSPACE_DIR}/{}", path.trim_start_matches("./"))
    }
}

fn split(path: &str) -> Result<(&str, &str)> {
    let trimmed = path.trim_end_matches('/');
    match trimmed.rfind('/') {
        Some(i) if i + 1 < trimmed.len() => {
            let dir = if i == 0 { "/" } else { &trimmed[..i] };
            Ok((dir, &trimmed[i + 1..]))
        }
        _ => Err(Error::new(
            ErrorCode::InvalidRequest,
            format!("not a file path: {path}"),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resolves_and_splits() {
        assert_eq!(resolve_path("a/b.txt"), "/root/workspace/a/b.txt");
        assert_eq!(resolve_path("./a"), "/root/workspace/a");
        assert_eq!(resolve_path("/etc/x"), "/etc/x");
        assert_eq!(split("/etc/x").unwrap(), ("/etc", "x"));
        assert_eq!(split("/x").unwrap(), ("/", "x"));
        assert!(split("/").is_err());
    }
}

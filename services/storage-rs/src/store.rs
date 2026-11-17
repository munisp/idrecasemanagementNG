// Author: Patrick Munis at newwave
//! Core object operations over an OpenDAL operator — kept free of axum so
//! every behavior here is unit-testable against the in-memory backend.
//!
//! Logical layout: `"{bucket}/{key}"` under the operator root (buckets are
//! key namespaces, so one backend serves every logical bucket). User
//! metadata travels in a JSON sidecar at `"{key}.__meta.json"` so it
//! survives backends without native metadata (fs, memory).
//!
//! Multipart uploads are modeled as a staging prefix
//! `"{bucket}/.uploads/{upload_id}/"` holding one object per part plus a
//! `session.json`; completion concatenates parts in part-number order
//! through a streaming writer and removes the staging prefix. This works
//! identically on every backend, including ones without native multipart.

use bytes::Bytes;
use opendal::{ErrorKind, Operator};
use serde::{Deserialize, Serialize};

#[derive(Debug)]
pub enum StoreError {
    NotFound(String),
    Invalid(String),
    Backend(String),
}

impl StoreError {
    fn of(op: &'static str, path: &str, e: opendal::Error) -> Self {
        match e.kind() {
            ErrorKind::NotFound => StoreError::NotFound(format!("{op} {path}: not found")),
            _ => StoreError::Backend(format!("{op} {path}: {e}")),
        }
    }
}

impl std::fmt::Display for StoreError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            StoreError::NotFound(m) | StoreError::Invalid(m) | StoreError::Backend(m) => {
                f.write_str(m)
            }
        }
    }
}

#[derive(Clone)]
pub struct Store {
    op: Operator,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ObjectMeta {
    pub size: u64,
    #[serde(default)]
    pub etag: String,
    #[serde(default)]
    pub content_type: String,
    #[serde(default)]
    pub user: std::collections::BTreeMap<String, String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct Session {
    key: String,
    content_type: String,
    user: std::collections::BTreeMap<String, String>,
}

const META_SUFFIX: &str = ".__meta.json";
const UPLOADS: &str = ".uploads";

/// Reject anything that could escape the bucket namespace — the fs backend
/// resolves keys onto a real filesystem, so `..` segments are a traversal.
fn join_key(bucket: &str, key: &str) -> Result<String, StoreError> {
    let bad = |what: &str| {
        what.is_empty()
            || what.starts_with('/')
            || what.ends_with('/')
            || what.split('/').any(|seg| seg.is_empty() || seg == "." || seg == "..")
    };
    if bad(bucket) || bad(key) {
        return Err(StoreError::Invalid(format!("bad bucket/key: {bucket:?}/{key:?}")));
    }
    Ok(format!("{bucket}/{key}"))
}

fn uploads_prefix(bucket: &str, upload_id: &str) -> Result<String, StoreError> {
    if upload_id.is_empty() || !upload_id.chars().all(|c| c.is_ascii_hexdigit()) {
        return Err(StoreError::Invalid(format!("bad upload id {upload_id:?}")));
    }
    if bucket.is_empty()
        || bucket.starts_with('/')
        || bucket.split('/').any(|s| s.is_empty() || s == "." || s == "..")
    {
        return Err(StoreError::Invalid(format!("bad bucket {bucket:?}")));
    }
    Ok(format!("{bucket}/{UPLOADS}/{upload_id}/"))
}

impl Store {
    pub fn new(op: Operator) -> Self {
        Store { op }
    }

    async fn write_meta(
        &self,
        full: &str,
        content_type: &str,
        user: std::collections::BTreeMap<String, String>,
    ) -> Result<(), StoreError> {
        if content_type.is_empty() && user.is_empty() {
            return Ok(());
        }
        let meta = serde_json::json!({ "content_type": content_type, "user": user });
        let path = format!("{full}{META_SUFFIX}");
        self.op
            .write(&path, meta.to_string())
            .await
            .map_err(|e| StoreError::of("write_meta", &path, e))?;
        Ok(())
    }

    async fn read_meta(&self, full: &str) -> (String, std::collections::BTreeMap<String, String>) {
        let path = format!("{full}{META_SUFFIX}");
        match self.op.read(&path).await {
            Ok(buf) => serde_json::from_slice::<serde_json::Value>(&buf.to_bytes())
                .map(|v| {
                    (
                        v["content_type"].as_str().unwrap_or("").to_string(),
                        serde_json::from_value(v["user"].clone()).unwrap_or_default(),
                    )
                })
                .unwrap_or_default(),
            Err(_) => Default::default(),
        }
    }

    pub async fn put(
        &self,
        bucket: &str,
        key: &str,
        data: Bytes,
        content_type: &str,
        user: std::collections::BTreeMap<String, String>,
    ) -> Result<(), StoreError> {
        let full = join_key(bucket, key)?;
        self.op
            .write(&full, data)
            .await
            .map_err(|e| StoreError::of("put", &full, e))?;
        self.write_meta(&full, content_type, user).await
    }

    pub async fn get(&self, bucket: &str, key: &str) -> Result<Bytes, StoreError> {
        let full = join_key(bucket, key)?;
        self.op
            .read(&full)
            .await
            .map(|b| b.to_bytes())
            .map_err(|e| StoreError::of("get", &full, e))
    }

    pub async fn get_range(
        &self,
        bucket: &str,
        key: &str,
        start: u64,
        end_inclusive: Option<u64>,
    ) -> Result<(Bytes, u64), StoreError> {
        let full = join_key(bucket, key)?;
        let total = self
            .op
            .stat(&full)
            .await
            .map_err(|e| StoreError::of("stat", &full, e))?
            .content_length();
        if start >= total && total > 0 {
            return Err(StoreError::Invalid(format!(
                "range start {start} beyond object size {total}"
            )));
        }
        let buf = match end_inclusive {
            Some(end) if end >= start => self
                .op
                .read_with(&full)
                .range(start..(end + 1).min(total))
                .await,
            _ => self.op.read_with(&full).range(start..).await,
        }
        .map_err(|e| StoreError::of("get_range", &full, e))?;
        Ok((buf.to_bytes(), total))
    }

    pub async fn stat(&self, bucket: &str, key: &str) -> Result<ObjectMeta, StoreError> {
        let full = join_key(bucket, key)?;
        let m = self
            .op
            .stat(&full)
            .await
            .map_err(|e| StoreError::of("stat", &full, e))?;
        let (content_type, user) = self.read_meta(&full).await;
        Ok(ObjectMeta {
            size: m.content_length(),
            etag: m.etag().unwrap_or("").to_string(),
            content_type,
            user,
        })
    }

    pub async fn remove(&self, bucket: &str, key: &str) -> Result<(), StoreError> {
        let full = join_key(bucket, key)?;
        self.op
            .delete(&full)
            .await
            .map_err(|e| StoreError::of("delete", &full, e))?;
        // sidecar is best-effort: a missing one is not an error
        let _ = self.op.delete(&format!("{full}{META_SUFFIX}")).await;
        Ok(())
    }

    pub async fn list(&self, bucket: &str, prefix: &str) -> Result<Vec<String>, StoreError> {
        if bucket.is_empty()
            || bucket.starts_with('/')
            || bucket.split('/').any(|s| s.is_empty() || s == "." || s == "..")
        {
            return Err(StoreError::Invalid(format!("bad bucket {bucket:?}")));
        }
        let base = if prefix.is_empty() {
            format!("{bucket}/")
        } else {
            join_key(bucket, prefix)?
        };
        let entries = self
            .op
            .list_with(&base)
            .recursive(true)
            .await
            .map_err(|e| StoreError::of("list", &base, e))?;
        Ok(entries
            .into_iter()
            .map(|e| e.path().to_string())
            .filter(|p| {
                !p.ends_with(META_SUFFIX) && !p.ends_with('/') && !p.contains(&format!("/{UPLOADS}/"))
            })
            .collect())
    }

    // ---- multipart -------------------------------------------------------

    pub async fn upload_begin(
        &self,
        bucket: &str,
        key: &str,
        content_type: &str,
        user: std::collections::BTreeMap<String, String>,
    ) -> Result<String, StoreError> {
        let mut id = [0u8; 16];
        rand::RngCore::fill_bytes(&mut rand::thread_rng(), &mut id);
        let upload_id = hex::encode(id);
        let prefix = uploads_prefix(bucket, &upload_id)?;
        let session = Session {
            key: key.to_string(),
            content_type: content_type.to_string(),
            user,
        };
        let body = serde_json::to_vec(&session)
            .map_err(|e| StoreError::Invalid(format!("session encode: {e}")))?;
        let path = format!("{prefix}session.json");
        self.op
            .write(&path, body)
            .await
            .map_err(|e| StoreError::of("upload_begin", &path, e))?;
        Ok(upload_id)
    }

    pub async fn upload_part(
        &self,
        bucket: &str,
        upload_id: &str,
        part_no: u32,
        data: Bytes,
    ) -> Result<u64, StoreError> {
        let prefix = uploads_prefix(bucket, upload_id)?;
        if part_no == 0 {
            return Err(StoreError::Invalid("part numbers start at 1".into()));
        }
        // the session must exist — parts without a session are orphans
        let session_path = format!("{prefix}session.json");
        self.op
            .stat(&session_path)
            .await
            .map_err(|e| StoreError::of("upload_part session", &session_path, e))?;
        let path = format!("{prefix}{part_no:08}.part");
        let n = data.len() as u64;
        self.op
            .write(&path, data)
            .await
            .map_err(|e| StoreError::of("upload_part", &path, e))?;
        Ok(n)
    }

    pub async fn upload_complete(&self, bucket: &str, upload_id: &str) -> Result<ObjectMeta, StoreError> {
        let prefix = uploads_prefix(bucket, upload_id)?;
        let session_path = format!("{prefix}session.json");
        let session: Session = self
            .op
            .read(&session_path)
            .await
            .map_err(|e| StoreError::of("upload_complete session", &session_path, e))
            .and_then(|b| {
                serde_json::from_slice(&b.to_bytes())
                    .map_err(|e| StoreError::Invalid(format!("session decode: {e}")))
            })?;

        let mut parts: Vec<String> = self
            .op
            .list_with(&prefix)
            .await
            .map_err(|e| StoreError::of("upload_complete list", &prefix, e))?
            .into_iter()
            .map(|e| e.path().to_string())
            .filter(|p| p.ends_with(".part"))
            .collect();
        if parts.is_empty() {
            return Err(StoreError::Invalid("no parts uploaded".into()));
        }
        parts.sort(); // zero-padded part numbers sort into upload order

        let full = join_key(bucket, &session.key)?;
        let mut writer = self
            .op
            .writer(&full)
            .await
            .map_err(|e| StoreError::of("upload_complete open", &full, e))?;
        let mut total = 0u64;
        for part in &parts {
            let buf = self
                .op
                .read(part)
                .await
                .map_err(|e| StoreError::of("upload_complete read", part, e))?;
            let b = buf.to_bytes();
            total += b.len() as u64;
            writer
                .write(b)
                .await
                .map_err(|e| StoreError::of("upload_complete write", &full, e))?;
        }
        writer
            .close()
            .await
            .map_err(|e| StoreError::of("upload_complete close", &full, e))?;
        self.write_meta(&full, &session.content_type, session.user).await?;
        // staging cleanup is mandatory for the immutable-archive discipline:
        // parts are not documents and must not linger
        let _ = self.op.delete_with(&prefix).recursive(true).await;
        self.stat(bucket, &session.key).await.map(|mut m| {
            m.size = total;
            m
        })
    }

    pub async fn upload_abort(&self, bucket: &str, upload_id: &str) -> Result<(), StoreError> {
        let prefix = uploads_prefix(bucket, upload_id)?;
        self.op
            .delete_with(&prefix)
            .recursive(true)
            .await
            .map_err(|e| StoreError::of("upload_abort", &prefix, e))
    }
}

#[cfg(test)]
mod tests {
    use opendal::services;
    use super::*;

    fn store() -> Store {
        Store::new(Operator::new(services::Memory::default()).unwrap())
    }

    #[tokio::test]
    async fn put_get_stat_remove() {
        let s = store();
        let mut user = std::collections::BTreeMap::new();
        user.insert("tenant".to_string(), "tx".to_string());
        s.put("docs", "tx/cases/1/a.enc", Bytes::from("ciphertext"), "application/octet-stream", user)
            .await
            .unwrap();
        assert_eq!(s.get("docs", "tx/cases/1/a.enc").await.unwrap().as_ref(), b"ciphertext");
        let m = s.stat("docs", "tx/cases/1/a.enc").await.unwrap();
        assert_eq!(m.size, 10);
        assert_eq!(m.content_type, "application/octet-stream");
        assert_eq!(m.user["tenant"], "tx");
        s.remove("docs", "tx/cases/1/a.enc").await.unwrap();
        assert!(matches!(
            s.stat("docs", "tx/cases/1/a.enc").await,
            Err(StoreError::NotFound(_))
        ));
    }

    #[tokio::test]
    async fn range_reads() {
        let s = store();
        s.put("docs", "k", Bytes::from("0123456789"), "", Default::default())
            .await
            .unwrap();
        let (b, total) = s.get_range("docs", "k", 2, Some(5)).await.unwrap();
        assert_eq!(b.as_ref(), b"2345");
        assert_eq!(total, 10);
        let (b, _) = s.get_range("docs", "k", 8, None).await.unwrap();
        assert_eq!(b.as_ref(), b"89");
        assert!(s.get_range("docs", "k", 10, None).await.is_err());
    }

    #[tokio::test]
    async fn traversal_rejected() {
        let s = store();
        assert!(s.put("docs", "../escape", Bytes::from("x"), "", Default::default()).await.is_err());
        assert!(s.put("docs", "a//b", Bytes::from("x"), "", Default::default()).await.is_err());
        assert!(s.get("docs", "/abs").await.is_err());
        assert!(s.put("docs", "ok/k", Bytes::from("x"), "", Default::default()).await.is_ok());
    }

    #[tokio::test]
    async fn multipart_concatenates_in_part_order() {
        let s = store();
        let id = s.upload_begin("docs", "big.enc", "application/octet-stream", Default::default()).await.unwrap();
        s.upload_part("docs", &id, 2, Bytes::from("WORLD")).await.unwrap();
        s.upload_part("docs", &id, 1, Bytes::from("HELLO ")).await.unwrap();
        let m = s.upload_complete("docs", &id).await.unwrap();
        assert_eq!(m.size, 11);
        assert_eq!(s.get("docs", "big.enc").await.unwrap().as_ref(), b"HELLO WORLD");
        // staging prefix is gone
        assert!(s.list("docs", ".uploads").await.unwrap().is_empty());
        // orphan parts rejected
        assert!(s.upload_part("docs", &id, 3, Bytes::from("x")).await.is_err());
    }

    #[tokio::test]
    async fn abort_clears_staging() {
        let s = store();
        let id = s.upload_begin("docs", "x", "", Default::default()).await.unwrap();
        s.upload_part("docs", &id, 1, Bytes::from("abc")).await.unwrap();
        s.upload_abort("docs", &id).await.unwrap();
        assert!(s.upload_part("docs", &id, 2, Bytes::from("d")).await.is_err());
    }
}

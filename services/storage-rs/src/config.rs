// Author: Patrick Munis at newwave
//! Backend selection: env → OpenDAL Operator. This is the ONLY place the
//! platform names a storage vendor — every service above talks to the
//! gateway REST API, so moving between MinIO, S3, GCS, Azure Blob, or a
//! local filesystem is an env change here, not a code change anywhere else.
//!
//!   OPENDAL_SCHEME    s3 (default) | fs | gcs | azblob | memory (tests)
//!   OPENDAL_ROOT      key prefix inside the backend (default "/")
//!   s3:     OPENDAL_BUCKET (required), OPENDAL_ENDPOINT, OPENDAL_REGION,
//!           OPENDAL_ACCESS_KEY_ID, OPENDAL_SECRET_ACCESS_KEY
//!   fs:     OPENDAL_ROOT must be an absolute host path (e.g. /data)
//!   gcs:    OPENDAL_BUCKET, OPENDAL_CREDENTIAL_PATH
//!   azblob: OPENDAL_CONTAINER, OPENDAL_ACCOUNT_NAME, OPENDAL_ACCOUNT_KEY,
//!           OPENDAL_ENDPOINT

use opendal::{services, Operator};

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Scheme {
    S3,
    Fs,
    Gcs,
    Azblob,
    Memory,
}

pub fn parse_scheme(s: &str) -> Result<Scheme, String> {
    match s.to_ascii_lowercase().as_str() {
        "s3" => Ok(Scheme::S3),
        "fs" => Ok(Scheme::Fs),
        "gcs" => Ok(Scheme::Gcs),
        "azblob" | "azure" => Ok(Scheme::Azblob),
        "memory" => Ok(Scheme::Memory),
        other => Err(format!(
            "unknown OPENDAL_SCHEME {other:?} (want s3|fs|gcs|azblob|memory)"
        )),
    }
}

fn env(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|v| !v.is_empty())
}

pub fn operator_from_env() -> Result<Operator, String> {
    let scheme = parse_scheme(&env("OPENDAL_SCHEME").unwrap_or_else(|| "s3".into()))?;
    let root = env("OPENDAL_ROOT").unwrap_or_else(|| "/".into());

    let mut builder = match scheme {
        Scheme::S3 => {
            let bucket = env("OPENDAL_BUCKET")
                .ok_or("OPENDAL_BUCKET required for s3 scheme")?;
            let mut b = services::S3::default()
                .bucket(&bucket)
                .region(&env("OPENDAL_REGION").unwrap_or_else(|| "us-east-1".into()))
                .root(&root);
            if let Some(ep) = env("OPENDAL_ENDPOINT") {
                b = b.endpoint(&ep);
            }
            if let (Some(k), Some(s)) =
                (env("OPENDAL_ACCESS_KEY_ID"), env("OPENDAL_SECRET_ACCESS_KEY"))
            {
                b = b.access_key_id(&k).secret_access_key(&s);
            }
            Operator::new(b)
        }
        Scheme::Fs => {
            if !root.starts_with('/') {
                return Err("OPENDAL_ROOT must be an absolute path for fs scheme".into());
            }
            Operator::new(services::Fs::default().root(&root))
        }
        Scheme::Gcs => {
            #[cfg(feature = "gcs")]
            {
                let bucket = env("OPENDAL_BUCKET")
                    .ok_or("OPENDAL_BUCKET required for gcs scheme")?;
                let mut b = services::Gcs::default().bucket(&bucket).root(&root);
                if let Some(p) = env("OPENDAL_CREDENTIAL_PATH") {
                    b = b.credential_path(&p);
                }
                Operator::new(b)
            }
            #[cfg(not(feature = "gcs"))]
            return Err("built without gcs support — rebuild with --features gcs".into());
        }
        Scheme::Azblob => {
            #[cfg(feature = "azblob")]
            {
                let container = env("OPENDAL_CONTAINER")
                    .ok_or("OPENDAL_CONTAINER required for azblob scheme")?;
                let mut b = services::Azblob::default().container(&container).root(&root);
                if let (Some(n), Some(k)) =
                    (env("OPENDAL_ACCOUNT_NAME"), env("OPENDAL_ACCOUNT_KEY"))
                {
                    b = b.account_name(&n).account_key(&k);
                }
                if let Some(ep) = env("OPENDAL_ENDPOINT") {
                    b = b.endpoint(&ep);
                }
                Operator::new(b)
            }
            #[cfg(not(feature = "azblob"))]
            return Err("built without azblob support — rebuild with --features azblob".into());
        }
        Scheme::Memory => Operator::new(services::Memory::default()),
    }
    .map_err(|e| format!("opendal operator build: {e}"))?;

    // Bounded retries with jitter — transient object-store blips must not
    // bubble up as document-write failures on the docket.
    builder = builder.layer(opendal::layers::RetryLayer::new().with_jitter());
    Ok(builder)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn scheme_parsing() {
        assert_eq!(parse_scheme("s3").unwrap(), Scheme::S3);
        assert_eq!(parse_scheme("FS").unwrap(), Scheme::Fs);
        assert_eq!(parse_scheme("azure").unwrap(), Scheme::Azblob);
        assert_eq!(parse_scheme("memory").unwrap(), Scheme::Memory);
        assert!(parse_scheme("ftp").is_err());
    }

    #[test]
    fn memory_operator_roundtrips() {
        // OPENDAL_SCHEME=memory needs no credentials — proof the whole stack
        // above the operator is backend-agnostic.
        std::env::set_var("OPENDAL_SCHEME", "memory");
        let op = operator_from_env().unwrap();
        let rt = tokio::runtime::Runtime::new().unwrap();
        rt.block_on(async {
            op.write("b/k.txt", "hello").await.unwrap();
            let got = op.read("b/k.txt").await.unwrap();
            assert_eq!(got.to_bytes().as_ref(), b"hello");
            assert_eq!(op.stat("b/k.txt").await.unwrap().content_length(), 5);
            op.delete("b/k.txt").await.unwrap();
            assert!(!op.exists("b/k.txt").await.unwrap());
        });
        std::env::remove_var("OPENDAL_SCHEME");
    }
}

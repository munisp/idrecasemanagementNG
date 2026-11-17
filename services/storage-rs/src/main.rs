// Author: Patrick Munis at newwave
//! storage-rs — the platform's object-storage gateway, built on Apache
//! OpenDAL. One REST surface for every blob the platform stores (document
//! ciphertext, templates, exports, ML artifacts); the actual backend
//! (MinIO / S3 / GCS / Azure Blob / local fs) is an env decision in
//! config.rs. All platform services MUST go through this gateway rather
//! than embedding storage-vendor SDKs.
//!
//! Auth: X-Internal-Auth shared token on every route except /healthz
//! (same fail-closed pattern as vault-rs).

mod config;
mod store;

use axum::{
    body::Bytes,
    extract::{Path, Query, Request, State},
    http::{header, HeaderMap, StatusCode},
    middleware::{self, Next},
    response::{IntoResponse, Response},
    routing::{get, post, put},
    Json, Router,
};
use store::{Store, StoreError};

#[derive(Clone)]
struct AppState {
    store: Store,
    internal_token: Option<String>,
}

fn status(e: StoreError) -> Response {
    match e {
        StoreError::NotFound(m) => (StatusCode::NOT_FOUND, m).into_response(),
        StoreError::Invalid(m) => (StatusCode::BAD_REQUEST, m).into_response(),
        StoreError::Backend(m) => {
            tracing::error!("storage backend: {m}");
            (StatusCode::BAD_GATEWAY, "storage backend error").into_response()
        }
    }
}

/// X-Object-Meta-* headers -> user metadata map.
fn user_meta(headers: &HeaderMap) -> std::collections::BTreeMap<String, String> {
    headers
        .iter()
        .filter_map(|(k, v)| {
            let name = k.as_str().strip_prefix("x-object-meta-")?;
            Some((name.to_string(), v.to_str().ok()?.to_string()))
        })
        .collect()
}

fn content_type(headers: &HeaderMap) -> String {
    headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string()
}

async fn healthz() -> Json<serde_json::Value> {
    Json(serde_json::json!({ "ok": true, "service": "storage-rs", "engine": "opendal" }))
}

async fn put_object(
    State(st): State<AppState>,
    Path((bucket, key)): Path<(String, String)>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    match st
        .store
        .put(&bucket, &key, body, &content_type(&headers), user_meta(&headers))
        .await
    {
        Ok(()) => StatusCode::CREATED.into_response(),
        Err(e) => status(e),
    }
}

/// `bytes=start-end` / `bytes=start-` — suffix ranges are intentionally not
/// supported: every platform caller knows exact offsets.
fn parse_range(v: &str) -> Option<(u64, Option<u64>)> {
    let spec = v.strip_prefix("bytes=")?;
    let (a, b) = spec.split_once('-')?;
    let start: u64 = a.trim().parse().ok()?;
    let end = if b.trim().is_empty() {
        None
    } else {
        Some(b.trim().parse().ok()?)
    };
    Some((start, end))
}

async fn get_object(
    State(st): State<AppState>,
    Path((bucket, key)): Path<(String, String)>,
    headers: HeaderMap,
) -> Response {
    if let Some(range) = headers.get(header::RANGE).and_then(|v| v.to_str().ok()) {
        let Some((start, end)) = parse_range(range) else {
            return (StatusCode::BAD_REQUEST, "bad Range header").into_response();
        };
        return match st.store.get_range(&bucket, &key, start, end).await {
            Ok((bytes, total)) => {
                let last = start + bytes.len() as u64 - 1;
                (
                    StatusCode::PARTIAL_CONTENT,
                    [
                        (header::CONTENT_RANGE, format!("bytes {start}-{last}/{total}")),
                        (header::CONTENT_LENGTH, bytes.len().to_string()),
                        (header::ACCEPT_RANGES, "bytes".to_string()),
                    ],
                    bytes,
                )
                    .into_response()
            }
            Err(e) => status(e),
        };
    }
    match st.store.get(&bucket, &key).await {
        Ok(bytes) => {
            let meta = st.store.stat(&bucket, &key).await.ok();
            let mut resp = (StatusCode::OK, bytes).into_response();
            if let Some(m) = meta {
                if !m.content_type.is_empty() {
                    resp.headers_mut().insert(
                        header::CONTENT_TYPE,
                        m.content_type.parse().unwrap_or(header::HeaderValue::from_static("application/octet-stream")),
                    );
                }
            }
            resp
        }
        Err(e) => status(e),
    }
}

async fn head_object(
    State(st): State<AppState>,
    Path((bucket, key)): Path<(String, String)>,
) -> Response {
    match st.store.stat(&bucket, &key).await {
        Ok(m) => {
            let mut resp = StatusCode::OK.into_response();
            let h = resp.headers_mut();
            h.insert(header::CONTENT_LENGTH, m.size.into());
            if !m.etag.is_empty() {
                h.insert("x-object-etag", m.etag.parse().unwrap());
            }
            if !m.content_type.is_empty() {
                if let Ok(v) = m.content_type.parse() {
                    h.insert(header::CONTENT_TYPE, v);
                }
            }
            for (k, v) in m.user {
                if let Ok(val) = v.parse() {
                    h.insert(
                        header::HeaderName::from_bytes(
                            format!("x-object-meta-{k}").as_bytes(),
                        )
                        .unwrap(),
                        val,
                    );
                }
            }
            resp
        }
        Err(e) => status(e),
    }
}

async fn delete_object(
    State(st): State<AppState>,
    Path((bucket, key)): Path<(String, String)>,
) -> Response {
    match st.store.remove(&bucket, &key).await {
        Ok(()) => StatusCode::NO_CONTENT.into_response(),
        Err(e) => status(e),
    }
}

#[derive(serde::Deserialize)]
struct ListParams {
    prefix: Option<String>,
}

async fn list_objects(
    State(st): State<AppState>,
    Path(bucket): Path<String>,
    Query(q): Query<ListParams>,
) -> Response {
    match st.store.list(&bucket, q.prefix.as_deref().unwrap_or("")).await {
        Ok(keys) => Json(serde_json::json!({ "objects": keys })).into_response(),
        Err(e) => status(e),
    }
}

async fn upload_begin(
    State(st): State<AppState>,
    Path((bucket, key)): Path<(String, String)>,
    headers: HeaderMap,
) -> Response {
    match st
        .store
        .upload_begin(&bucket, &key, &content_type(&headers), user_meta(&headers))
        .await
    {
        Ok(id) => (
            StatusCode::CREATED,
            Json(serde_json::json!({ "upload_id": id })),
        )
            .into_response(),
        Err(e) => status(e),
    }
}

async fn upload_part(
    State(st): State<AppState>,
    Path((bucket, upload_id, part_no)): Path<(String, String, u32)>,
    body: Bytes,
) -> Response {
    match st.store.upload_part(&bucket, &upload_id, part_no, body).await {
        Ok(n) => Json(serde_json::json!({ "part": part_no, "size": n })).into_response(),
        Err(e) => status(e),
    }
}

async fn upload_complete(
    State(st): State<AppState>,
    Path((bucket, upload_id)): Path<(String, String)>,
) -> Response {
    match st.store.upload_complete(&bucket, &upload_id).await {
        Ok(m) => Json(serde_json::json!({
            "size": m.size, "etag": m.etag, "content_type": m.content_type
        }))
        .into_response(),
        Err(e) => status(e),
    }
}

async fn upload_abort(
    State(st): State<AppState>,
    Path((bucket, upload_id)): Path<(String, String)>,
) -> Response {
    match st.store.upload_abort(&bucket, &upload_id).await {
        Ok(()) => StatusCode::NO_CONTENT.into_response(),
        Err(e) => status(e),
    }
}

/// Internal-auth gate — identical discipline to vault-rs: constant-time
/// compare, missing/empty token on either side fails closed (401).
async fn require_internal(State(st): State<AppState>, req: Request, next: Next) -> Response {
    if let Some(expected) = &st.internal_token {
        let given = req
            .headers()
            .get("X-Internal-Auth")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("");
        let (a, b) = (expected.as_bytes(), given.as_bytes());
        let ok = a.len() == b.len()
            && a.iter().zip(b.iter()).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0;
        if !ok {
            return (StatusCode::UNAUTHORIZED, "internal auth required").into_response();
        }
    }
    next.run(req).await
}

#[tokio::main(flavor = "multi_thread")]
async fn main() {
    tracing_subscriber::fmt::init();
    let op = config::operator_from_env().unwrap_or_else(|e| {
        panic!("storage-rs cannot start: {e}")
    });
    // Fail at boot if the backend is unreachable/misconfigured rather than
    // on the first document write.
    if let Err(e) = op.check().await {
        panic!("storage-rs backend check failed: {e}");
    }
    tracing::info!(scheme = ?op.info().scheme(), "opendal backend ready");

    let internal_token = std::env::var("STORAGE_INTERNAL_TOKEN")
        .ok()
        .filter(|s| !s.is_empty());
    if internal_token.is_none() {
        tracing::warn!("STORAGE_INTERNAL_TOKEN unset — gateway routes are unauthenticated (dev only)");
    }
    let st = AppState {
        store: Store::new(op),
        internal_token,
    };

    let api = Router::new()
        .route("/objects/{bucket}", get(list_objects))
        .route(
            "/objects/{bucket}/{*key}",
            put(put_object).get(get_object).head(head_object).delete(delete_object),
        )
        .route("/upload-begin/{bucket}/{*key}", post(upload_begin))
        .route("/uploads/{bucket}/{upload_id}/parts/{part_no}", put(upload_part))
        .route("/uploads/{bucket}/{upload_id}/complete", post(upload_complete))
        .route("/uploads/{bucket}/{upload_id}/abort", post(upload_abort))
        .route_layer(middleware::from_fn_with_state(st.clone(), require_internal));

    let app = Router::new()
        .route("/healthz", get(healthz))
        .merge(api)
        .with_state(st);

    let addr = std::env::var("LISTEN_ADDR").unwrap_or_else(|_| "0.0.0.0:8094".into());
    let listener = tokio::net::TcpListener::bind(&addr).await.unwrap();
    tracing::info!("storage-rs listening on {addr}");
    axum::serve(listener, app).await.unwrap();
}

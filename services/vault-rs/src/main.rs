//! vault-rs — cryptographic trust boundary for the double-blind IDR offer vault.
//!
//! Design (see docs/ARCHITECTURE.md §8):
//!   master secret (env/KMS) --HKDF-SHA256--> per-tenant data key
//!   per-offer random DEK --AES-256-GCM--> ciphertext
//!   DEK wrapped with tenant data key; DB stores only (nonce, ct, wrapped_dek).
//!
//! Reveal is a LAWFUL-TRANSITION protocol: the vault verifies the case's
//! workflow state (via case-api, which queries Temporal) before decrypting.
//! Tampering with DB flags alone cannot reveal an offer.

use aes_gcm::{
    aead::{Aead, KeyInit, Payload},
    Aes256Gcm, Nonce,
};
use axum::{
    extract::State,
    http::StatusCode,
    response::IntoResponse,
    routing::post,
    Json, Router,
};
use base64::{engine::general_purpose::STANDARD as B64, Engine};
use hkdf::Hkdf;
use rand::RngCore;
use serde::{Deserialize, Serialize};
use sha2::Sha256;
use std::sync::Arc;
use zeroize::Zeroize;

#[derive(Clone)]
struct AppState {
    master: Arc<[u8; 32]>,
    case_api: String, // e.g. http://case-api:8080 — reveal-state oracle
    http: reqwest::Client,
}

#[derive(Deserialize)]
struct SealRequest {
    tenant: String,        // two-letter state code
    case_id: String,
    offer_id: String,
    party_id: String,
    plaintext_b64: String, // offer payload (amount + justification refs)
}

#[derive(Serialize)]
struct SealResponse {
    nonce_b64: String,
    ciphertext_b64: String,
    wrapped_dek_b64: String,
}

#[derive(Deserialize)]
struct RevealRequest {
    tenant: String,
    case_id: String,
    offer_id: String,
    nonce_b64: String,
    ciphertext_b64: String,
    wrapped_dek_b64: String,
    caller_role: String, // ARBITRATOR | CASE_MANAGER | SYSTEM
}

#[derive(Serialize)]
struct RevealResponse {
    plaintext_b64: String,
    reveal_reason: String,
}

#[derive(Deserialize)]
struct DocRequest {
    tenant: String,
    key: String, // object key under tenant prefix
    data_b64: String,
}

fn tenant_data_key(master: &[u8; 32], tenant: &str) -> [u8; 32] {
    let hk = Hkdf::<Sha256>::new(None, master);
    let mut okm = [0u8; 32];
    hk.expand(format!("idre-tenant-data-key:{tenant}").as_bytes(), &mut okm)
        .expect("HKDF expand");
    okm
}

fn wrap_dek(tenant_key: &[u8; 32], dek: &[u8; 32]) -> (Vec<u8>, [u8; 12]) {
    let cipher = Aes256Gcm::new_from_slice(tenant_key).expect("key");
    let mut nonce = [0u8; 12];
    rand::thread_rng().fill_bytes(&mut nonce);
    let ct = cipher
        .encrypt(Nonce::from_slice(&nonce), dek.as_ref())
        .expect("wrap");
    (ct, nonce)
}

fn unwrap_dek(tenant_key: &[u8; 32], wrapped: &[u8], nonce: &[u8; 12]) -> Result<[u8; 32], ()> {
    let cipher = Aes256Gcm::new_from_slice(tenant_key).map_err(|_| ())?;
    let pt = cipher.decrypt(Nonce::from_slice(nonce), wrapped).map_err(|_| ())?;
    let mut dek = [0u8; 32];
    dek.copy_from_slice(&pt);
    Ok(dek)
}

async fn seal(State(st): State<AppState>, Json(req): Json<SealRequest>) -> impl IntoResponse {
    let tkey = tenant_data_key(&st.master, &req.tenant);
    let mut dek = [0u8; 32];
    rand::thread_rng().fill_bytes(&mut dek);

    let pt = match B64.decode(&req.plaintext_b64) {
        Ok(p) => p,
        Err(_) => return (StatusCode::BAD_REQUEST, "bad base64").into_response(),
    };
    let cipher = Aes256Gcm::new_from_slice(&dek).expect("key");
    let mut nonce = [0u8; 12];
    rand::thread_rng().fill_bytes(&mut nonce);
    // AAD binds ciphertext to tenant/case/offer — cross-case swap attacks fail.
    let aad = format!("{}:{}:{}:{}", req.tenant, req.case_id, req.offer_id, req.party_id);
    let ct = match cipher.encrypt(
        Nonce::from_slice(&nonce),
        Payload { msg: &pt, aad: aad.as_bytes() },
    ) {
        Ok(c) => c,
        Err(_) => return (StatusCode::INTERNAL_SERVER_ERROR, "seal failed").into_response(),
    };
    let (wrapped, wrap_nonce) = wrap_dek(&tkey, &dek);
    // wrapped blob layout: [12-byte wrap nonce][wrapped DEK ciphertext]
    let mut wrapped_blob = Vec::with_capacity(12 + wrapped.len());
    wrapped_blob.extend_from_slice(&wrap_nonce);
    wrapped_blob.extend_from_slice(&wrapped);

    let mut dek_z = dek;
    dek_z.zeroize();

    Json(SealResponse {
        nonce_b64: B64.encode(nonce),
        ciphertext_b64: B64.encode(ct),
        wrapped_dek_b64: B64.encode(wrapped_blob),
    })
    .into_response()
}

/// Lawful reveal: the vault itself confirms the workflow state before decrypting.
async fn reveal(State(st): State<AppState>, Json(req): Json<RevealRequest>) -> impl IntoResponse {
    if req.caller_role != "ARBITRATOR" && req.caller_role != "SYSTEM" {
        return (StatusCode::FORBIDDEN, "role may not reveal").into_response();
    }

    // Reveal-state oracle: case-api checks Temporal workflow history for one of
    // BOTH_SUBMITTED | WINDOW_EXPIRED | RULE_DEFAULT on this case.
    let url = format!("{}/internal/reveal-check/{}", st.case_api, req.case_id);
    let lawful = st
        .http
        .get(url)
        .header("X-Internal-Auth", std::env::var("VAULT_INTERNAL_TOKEN").unwrap_or_default())
        .send()
        .await
        .and_then(|r| r.error_for_status())
        .map(|r| r.headers().get("X-Reveal-Reason").cloned());
    let reason = match lawful {
        Ok(Some(h)) => h.to_str().unwrap_or("").to_string(),
        _ => return (StatusCode::CONFLICT, "reveal not lawful: window still sealed").into_response(),
    };

    let tkey = tenant_data_key(&st.master, &req.tenant);
    let wrapped = match B64.decode(&req.wrapped_dek_b64) {
        Ok(w) if w.len() > 12 => w,
        _ => return (StatusCode::BAD_REQUEST, "bad wrapped dek").into_response(),
    };
    let (wn, wct) = wrapped.split_at(12);
    let mut wnonce = [0u8; 12];
    wnonce.copy_from_slice(wn);
    let dek = match unwrap_dek(&tkey, wct, &wnonce) {
        Ok(d) => d,
        Err(_) => return (StatusCode::UNPROCESSABLE_ENTITY, "unwrap failed").into_response(),
    };

    let nonce = match B64.decode(&req.nonce_b64) {
        Ok(n) if n.len() == 12 => n,
        _ => return (StatusCode::BAD_REQUEST, "bad nonce").into_response(),
    };
    let ct = match B64.decode(&req.ciphertext_b64) {
        Ok(c) => c,
        _ => return (StatusCode::BAD_REQUEST, "bad ciphertext").into_response(),
    };
    let cipher = Aes256Gcm::new_from_slice(&dek).expect("key");
    let aad = format!("{}:{}:{}:", req.tenant, req.case_id, req.offer_id); // party validated upstream
    let pt = match cipher.decrypt(
        Nonce::from_slice(&nonce),
        Payload { msg: &ct, aad: aad.as_bytes() },
    ) {
        Ok(p) => p,
        Err(_) => return (StatusCode::UNPROCESSABLE_ENTITY, "decrypt failed").into_response(),
    };
    let mut dek_z = dek;
    dek_z.zeroize();

    Json(RevealResponse {
        plaintext_b64: B64.encode(pt),
        reveal_reason: reason,
    })
    .into_response()
}

/// Document encryption at rest: MinIO stores ciphertext only.
async fn seal_doc(State(st): State<AppState>, Json(req): Json<DocRequest>) -> impl IntoResponse {
    let tkey = tenant_data_key(&st.master, &req.tenant);
    let data = match B64.decode(&req.data_b64) {
        Ok(d) => d,
        Err(_) => return (StatusCode::BAD_REQUEST, "bad base64").into_response(),
    };
    let cipher = Aes256Gcm::new_from_slice(&tkey).expect("key");
    let mut nonce = [0u8; 12];
    rand::thread_rng().fill_bytes(&mut nonce);
    let ct = match cipher.encrypt(
        Nonce::from_slice(&nonce),
        Payload { msg: &data, aad: req.key.as_bytes() },
    ) {
        Ok(c) => c,
        Err(_) => return (StatusCode::INTERNAL_SERVER_ERROR, "seal failed").into_response(),
    };
    let mut blob = Vec::with_capacity(12 + ct.len());
    blob.extend_from_slice(&nonce);
    blob.extend_from_slice(&ct);
    Json(serde_json::json!({ "sealed_b64": B64.encode(blob) })).into_response()
}

async fn open_doc(State(st): State<AppState>, Json(req): Json<DocRequest>) -> impl IntoResponse {
    let tkey = tenant_data_key(&st.master, &req.tenant);
    let blob = match B64.decode(&req.data_b64) {
        Ok(b) if b.len() > 12 => b,
        _ => return (StatusCode::BAD_REQUEST, "bad blob").into_response(),
    };
    let (nonce, ct) = blob.split_at(12);
    let cipher = Aes256Gcm::new_from_slice(&tkey).expect("key");
    match cipher.decrypt(
        Nonce::from_slice(nonce),
        Payload { msg: ct, aad: req.key.as_bytes() },
    ) {
        Ok(pt) => Json(serde_json::json!({ "data_b64": B64.encode(pt) })).into_response(),
        Err(_) => (StatusCode::UNPROCESSABLE_ENTITY, "decrypt failed").into_response(),
    }
}

// Multi-thread runtime, one worker per core (explicit for auditability; the
// vault is CPU-bound on AEAD seal/open, so worker count must equal cores).
#[tokio::main(flavor = "multi_thread")]
async fn main() {
    tracing_subscriber::fmt::init();
    let master_hex = std::env::var("VAULT_MASTER_SECRET")
        .expect("VAULT_MASTER_SECRET (64 hex chars) required — inject via Dapr secret store");
    let raw = hex::decode(master_hex).expect("hex");
    assert_eq!(raw.len(), 32, "master must be 32 bytes");
    let mut master = [0u8; 32];
    master.copy_from_slice(&raw);

    let st = AppState {
        master: Arc::new(master),
        case_api: std::env::var("CASE_API_URL")
            .unwrap_or_else(|_| "http://localhost:8080".into()),
        http: reqwest::Client::new(),
    };

    let app = Router::new()
        .route("/seal", post(seal))
        .route("/reveal", post(reveal))
        .route("/docs/seal", post(seal_doc))
        .route("/docs/open", post(open_doc))
        .with_state(st);

    let addr = std::env::var("ADDR").unwrap_or_else(|_| "0.0.0.0:8081".into());
    tracing::info!("vault listening on {addr}");
    axum::serve(
        tokio::net::TcpListener::bind(&addr).await.expect("bind"),
        app,
    )
    .await
    .expect("serve");
}

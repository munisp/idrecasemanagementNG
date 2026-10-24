"""Ray: ML training + scoring for the IDRE platform.

- settlement_propensity: logistic model trained from silver/cases history
  (lakehouse_common.logistic_fit — deterministic full-batch GD, numpy only).
  Trained parameters persist to gold/ml_models/<model>/tenant=<st>/model.json;
  the Serve deployment loads them and refreshes periodically. Below
  MIN_TRAIN_SAMPLES labeled cases the trainer reports insufficient_data and
  leaves any existing model untouched, and the endpoint reports
  insufficient_data instead of scoring with untrained constants (the old
  version served a placeholder sigmoid and hardcoded outlier parameters).
- qpa_outlier_score: robust z-score vs QPA distribution (median/MAD),
  parameters likewise trained from silver and persisted.

Endpoints sit behind APISIX with Keycloak JWT validation.
"""

from __future__ import annotations

import json
import os
import time

import numpy as np
import ray
from ray import serve

from lakehouse_common import (
    MIN_TRAIN_SAMPLES, extract_case_features, logistic_fit,
    logistic_predict_proba,
)

S3_BUCKET = os.environ.get("LAKEHOUSE_BUCKET", "lakehouse")
S3_ENDPOINT = os.environ.get("S3_ENDPOINT", "minio:9000")
S3_ACCESS_KEY = os.environ.get("S3_ACCESS_KEY", "idre")
S3_SECRET_KEY = os.environ.get("S3_SECRET_KEY", "idre-secret")
MODEL_REFRESH_SECONDS = 300

ray.init(address=os.environ.get("RAY_ADDRESS", "auto"), ignore_reinit_error=True)


def _s3fs():
    import pyarrow.fs as fs
    return fs.S3FileSystem(access_key=S3_ACCESS_KEY, secret_key=S3_SECRET_KEY,
                           endpoint_override=S3_ENDPOINT, scheme="http",
                           region="us-east-1", allow_bucket_creation=False)


def _model_path(model: str, tenant: str) -> str:
    return f"{S3_BUCKET}/gold/ml_models/{model}/tenant={tenant}/model.json"


def persist_model(model: str, tenant: str, params: dict) -> None:
    with _s3fs().open_output_stream(_model_path(model, tenant)) as f:
        f.write(json.dumps(params).encode())


def load_model(model: str, tenant: str) -> dict | None:
    try:
        with _s3fs().open_input_file(_model_path(model, tenant)) as f:
            return json.loads(f.read().decode())
    except FileNotFoundError:
        return None


@ray.remote
def train_settlement_model(tenant: str) -> dict:
    """Train from silver/cases and persist. Returns a status dict; on
    insufficient data NO existing model is overwritten."""
    from deltalake import DeltaTable

    try:
        dt = DeltaTable(f"s3://{S3_BUCKET}/silver/cases",
                        storage_options={
                            "AWS_ENDPOINT_URL": f"http://{S3_ENDPOINT}",
                            "AWS_ACCESS_KEY_ID": S3_ACCESS_KEY,
                            "AWS_SECRET_ACCESS_KEY": S3_SECRET_KEY,
                            "AWS_REGION": "us-east-1",
                            "AWS_ALLOW_HTTP": "true",
                            "AWS_S3_ALLOW_UNSAFE_RENAME": "true",
                        })
        table = dt.to_pyarrow_table(partitions=[("tenant", "=", tenant)])
    except Exception as exc:
        return {"status": "error", "detail": f"silver read failed: {exc}"}

    X, y, n_open = extract_case_features(table.to_pylist())
    if len(y) < MIN_TRAIN_SAMPLES:
        return {"status": "insufficient_data", "n": len(y),
                "min_samples": MIN_TRAIN_SAMPLES, "open_cases": n_open}

    w, b, mu, sd = logistic_fit(np.array(X), np.array(y))
    params = {
        "model": "settlement_propensity", "tenant": tenant,
        "w": w.tolist(), "b": float(b), "mu": mu.tolist(), "sd": sd.tolist(),
        "features": ["qpa_usd", "offer_count", "event_count"],
        "n_train": len(y), "n_open_skipped": n_open,
        "trained_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    persist_model("settlement_propensity", tenant, params)
    return {"status": "trained", "n": len(y)}


@ray.remote
def train_outlier_model(tenant: str) -> dict:
    """Median/MAD of initiated-case QPA for the tenant, persisted."""
    from deltalake import DeltaTable

    dt = DeltaTable(f"s3://{S3_BUCKET}/silver/cases",
                    storage_options={
                        "AWS_ENDPOINT_URL": f"http://{S3_ENDPOINT}",
                        "AWS_ACCESS_KEY_ID": S3_ACCESS_KEY,
                        "AWS_SECRET_ACCESS_KEY": S3_SECRET_KEY,
                        "AWS_REGION": "us-east-1",
                        "AWS_ALLOW_HTTP": "true",
                        "AWS_S3_ALLOW_UNSAFE_RENAME": "true",
                    })
    table = dt.to_pyarrow_table(partitions=[("tenant", "=", tenant)])
    samples = []
    for etype, payload in zip(table.column("etype").to_pylist(),
                              table.column("payload").to_pylist()):
        if etype != "case.initiated":
            continue
        try:
            qpa = float(json.loads(payload).get("qpa_cents") or 0) / 100
        except (ValueError, TypeError):
            continue
        if qpa > 0:
            samples.append(qpa)
    if len(samples) < MIN_TRAIN_SAMPLES:
        return {"status": "insufficient_data", "n": len(samples),
                "min_samples": MIN_TRAIN_SAMPLES}
    arr = np.array(samples)
    med = float(np.median(arr))
    mad = float(np.median(np.abs(arr - med))) or 1.0
    params = {"model": "qpa_outlier", "tenant": tenant, "median": med,
              "mad": mad, "n_train": len(samples),
              "trained_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    persist_model("qpa_outlier", tenant, params)
    return {"status": "trained", "n": len(samples)}


class _ModelRefresher:
    """Loads persisted params at startup and re-reads them when stale, so a
    scheduled retrain reaches serving replicas without a redeploy."""

    def __init__(self, model: str):
        self.model = model
        self.params: dict | None = None
        self.loaded_at = 0.0
        self._refresh()

    def _refresh(self) -> None:
        params = load_model(self.model, os.environ.get("TENANT", ""))
        if params is not None:
            self.params = params
            self.loaded_at = time.monotonic()

    def current(self) -> dict | None:
        if time.monotonic() - self.loaded_at > MODEL_REFRESH_SECONDS:
            self._refresh()
        return self.params


@serve.deployment(route_prefix="/ml/qpa-outlier", num_replicas=2)
class QpaOutlier:
    def __init__(self):
        self.models = _ModelRefresher("qpa_outlier")

    async def __call__(self, request) -> dict:
        body = await request.json()
        params = self.models.current()
        if params is None:
            return {"status": "insufficient_data",
                    "min_samples": MIN_TRAIN_SAMPLES}
        amount = float(body["amount_usd"])
        z = abs(amount - params["median"]) / (1.4826 * params["mad"])
        return {"amount_usd": amount, "robust_z": round(z, 3),
                "outlier": bool(z > 3.5), "n_train": params["n_train"]}


@serve.deployment(route_prefix="/ml/settlement-propensity", num_replicas=1)
class SettlementPropensity:
    def __init__(self):
        self.models = _ModelRefresher("settlement_propensity")

    async def __call__(self, request) -> dict:
        body = await request.json()
        params = self.models.current()
        if params is None:
            return {"status": "insufficient_data",
                    "min_samples": MIN_TRAIN_SAMPLES}
        x = [[float(body.get("qpa_usd", 0)),
              float(body.get("offer_count", 0)),
              float(body.get("event_count", 0))]]
        p = logistic_predict_proba(np.array(x), np.array(params["w"]),
                                   params["b"], np.array(params["mu"]),
                                   np.array(params["sd"]))[0]
        return {"propensity": round(float(p), 3), "n_train": params["n_train"],
                "trained_at": params["trained_at"]}


if __name__ == "__main__":
    serve.start(http_options={"host": "0.0.0.0", "port": 8093})
    serve.run(QpaOutlier.bind(), SettlementPropensity.bind())
    print("Ray Serve: :8093/ml/qpa-outlier, :8093/ml/settlement-propensity")

# Author: Patrick Munis at newwave
"""GraphSAGE link prediction over the dispute graph — pure numpy, real
training (mean-aggregator message passing + manual backprop through a
dot-product decoder with negative sampling, binary cross-entropy loss).

Edges used for training:
    * explicit case-to-case edges from FalkorDB (RELATED_TO / DUPLICATE_OF /
      SAME_BATCH), weighted by r.score — KGQA feedback shows up here
    * party-mediated edges: two cases sharing a provider or a payer are
      joined by a weak (weight 0.3) structural edge

Node features: hashed one-hot buckets for status / service_line / plan_type,
log-scaled QPA, and degree — deterministic, no external embedding service.

Trained weights persist to LAKEHOUSE_DIR/gold/gnn_weights.npz so predictions
survive restarts and every score carries a model_version.
"""

from __future__ import annotations

import hashlib
import json
import os
import time
from pathlib import Path

import numpy as np

import graphdb
import lakehouse_io

LAKEHOUSE_DIR = Path(os.environ.get("LAKEHOUSE_DIR", "./lakehouse"))
WEIGHTS_PATH = LAKEHOUSE_DIR / "gold" / "gnn_weights.npz"
META_PATH = LAKEHOUSE_DIR / "gold" / "gnn_meta.json"

FEAT_DIM = 24   # 8 status buckets + 8 service-line + 6 plan-type + qpa + degree
HID_DIM = 32
EPOCHS = 120
LR = 0.05
NEG_RATIO = 2
MODEL_VERSION = "graphsage-np-1.0"


def _bucket(value: str, n: int) -> int:
    return int(hashlib.md5((value or "").encode()).hexdigest(), 16) % n


def build_dataset(tenant: str) -> dict:
    """graph -> gnn: pull nodes/edges from FalkorDB and tensorize."""
    cases = graphdb.case_index(tenant)
    ids = [c["id"] for c in cases]
    idx = {cid: i for i, cid in enumerate(ids)}
    n = len(ids)

    X = np.zeros((n, FEAT_DIM), dtype=np.float64)
    deg = np.zeros(n, dtype=np.float64)
    for i, c in enumerate(cases):
        X[i, _bucket(c["status"], 8)] = 1.0
        X[i, 8 + _bucket(c["service_line"], 8)] = 1.0
        X[i, 16 + _bucket(c["plan_type"], 6)] = 1.0
        X[i, 22] = np.log1p(max(0, int(c["qpa_cents"] or 0))) / 15.0

    pos: list[tuple[int, int, float]] = []
    for src, dst, _type, w in graphdb.case_edges(tenant):
        if src in idx and dst in idx:
            pos.append((idx[src], idx[dst], float(w)))
    # party-mediated weak edges
    by_party: dict[str, list[int]] = {}
    for cid, _t, party in graphdb.party_edges(tenant):
        if cid in idx:
            by_party.setdefault(party, []).append(idx[cid])
    for members in by_party.values():
        for a in range(len(members)):
            for b in range(a + 1, len(members)):
                pos.append((members[a], members[b], 0.3))

    A = np.zeros((n, n), dtype=np.float64)
    for i, j, w in pos:
        A[i, j] = A[j, i] = 1.0
        deg[i] += 1
        deg[j] += 1
    if n:
        X[:, 23] = deg / max(1.0, deg.max())
    A += np.eye(n)  # self-loops: keep each node's own features through aggregation
    rowsum = A.sum(1, keepdims=True)
    rowsum[rowsum == 0] = 1.0
    A_norm = A / rowsum  # mean aggregation

    return {"ids": ids, "idx": idx, "X": X, "A": A_norm, "pos": pos, "n": n}


class GraphSAGE:
    def __init__(self, rng: np.random.Generator):
        self.W1 = rng.normal(0, 0.1, (FEAT_DIM, HID_DIM))
        self.W2 = rng.normal(0, 0.1, (HID_DIM, HID_DIM))

    def forward(self, A, X):
        # residual GraphSAGE: layer 2 = own layer-1 features + aggregated mix,
        # which prevents the embedding collapse plain mean stacking causes
        # on small dense graphs.
        self._Z1 = A @ X @ self.W1
        self._H1 = np.maximum(self._Z1, 0)  # relu
        self._H2 = self._H1 + A @ self._H1 @ self.W2
        return self._H2

    def backward(self, A, X, dH2):
        # H2 = H1 + A @ H1 @ W2
        dA_H1 = dH2 @ self.W2.T
        dW2 = (A @ self._H1).T @ dH2
        dH1 = dH2 + A.T @ dA_H1
        dZ1 = dH1 * (self._Z1 > 0)
        dW1 = (A @ X).T @ dZ1
        return dW1, dW2

    def step(self, dW1, dW2, lr):
        # BCE gradient is already batch-averaged; apply L2 to keep weights small
        self.W1 -= lr * (dW1 + 1e-4 * self.W1)
        self.W2 -= lr * (dW2 + 1e-4 * self.W2)


def _bce_with_logits(z: np.ndarray, y: np.ndarray) -> tuple[float, np.ndarray]:
    z = np.clip(z, -6, 6)
    loss = np.mean(np.maximum(z, 0) - z * y + np.log1p(np.exp(-np.abs(z))))
    sig = 1.0 / (1.0 + np.exp(-z))
    return float(loss), (sig - y) / len(z)


def _cos_scores(H, src, dst):
    """Cosine-similarity logits (x4 for sigmoid headroom). Scale-invariant,
    which avoids the gradient starvation a raw dot product suffers when
    mean-aggregated embeddings are small."""
    a, b = H[src], H[dst]
    na = np.linalg.norm(a, axis=1, keepdims=True) + 1e-6
    nb = np.linalg.norm(b, axis=1, keepdims=True) + 1e-6
    cos = np.sum(a * b, axis=1, keepdims=True) / (na * nb)
    return 2.5 * cos[:, 0], a, b, na, nb


def _cos_backward(dz, a, b, na, nb, src, dst, like):
    """Exact gradient of 2.5*cos(H[src], H[dst]) wrt H rows."""
    dH = np.zeros_like(like)
    cos = np.sum(a * b, axis=1, keepdims=True) / (na * nb)
    ga = 2.5 * dz[:, None] * (b / (na * nb) - cos * a / (na * na))
    gb = 2.5 * dz[:, None] * (a / (na * nb) - cos * b / (nb * nb))
    np.add.at(dH, src, ga)
    np.add.at(dH, dst, gb)
    return dH


def train(tenant: str, epochs: int = EPOCHS, lr: float = LR) -> dict:
    ds = build_dataset(tenant)
    n = ds["n"]
    if n < 3:
        return {"tenant": tenant, "trained": False, "reason": "need >=3 cases", "cases": n}
    pos = ds["pos"]
    if not pos:
        return {"tenant": tenant, "trained": False, "reason": "no positive edges", "cases": n}

    rng = np.random.default_rng(42)
    model = GraphSAGE(rng)
    X, A = ds["X"], ds["A"]
    pos_arr = np.array([(i, j) for i, j, _w in pos], dtype=np.int64)
    weights = np.array([w for _i, _j, w in pos], dtype=np.float64)

    pos_set = {(int(a), int(b)) for a, b in pos_arr} | {(int(b), int(a)) for a, b in pos_arr}

    def sample_negatives(count: int) -> np.ndarray:
        out = []
        while len(out) < count:
            cand = rng.integers(0, n, size=(count * 2, 2))
            for a, b in cand:
                if a != b and (int(a), int(b)) not in pos_set:
                    out.append((int(a), int(b)))
                if len(out) == count:
                    break
        return np.array(out, dtype=np.int64)

    t0 = time.time()
    for epoch in range(epochs):
        H = model.forward(A, X)
        # negative sampling: uniform pairs excluding known positives
        n_neg = len(pos_arr) * NEG_RATIO
        negs = sample_negatives(n_neg)
        src = np.concatenate([pos_arr[:, 0], negs[:, 0]])
        dst = np.concatenate([pos_arr[:, 1], negs[:, 1]])
        y = np.concatenate([weights, np.zeros(n_neg)])
        z, ea, eb, na, nb = _cos_scores(H, src, dst)
        loss, dz = _bce_with_logits(z, y)
        dH = _cos_backward(dz, ea, eb, na, nb, src, dst, H)
        dW1, dW2 = model.backward(A, X, dH)
        model.step(dW1, dW2, lr)

    WEIGHTS_PATH.parent.mkdir(parents=True, exist_ok=True)
    np.savez(WEIGHTS_PATH, W1=model.W1, W2=model.W2, tenant=tenant)
    META_PATH.write_text(json.dumps({
        "tenant": tenant, "model_version": MODEL_VERSION, "cases": n,
        "positive_edges": len(pos), "epochs": epochs, "final_loss": round(loss, 5),
        "train_seconds": round(time.time() - t0, 3), "trained_at": int(time.time()),
    }))
    return {
        "tenant": tenant, "trained": True, "model_version": MODEL_VERSION,
        "cases": n, "positive_edges": len(pos), "epochs": epochs,
        "final_loss": round(loss, 5), "train_seconds": round(time.time() - t0, 3),
    }


def _load_model() -> GraphSAGE | None:
    if not WEIGHTS_PATH.exists():
        return None
    d = np.load(WEIGHTS_PATH)
    m = GraphSAGE(np.random.default_rng(0))
    m.W1, m.W2 = d["W1"], d["W2"]
    return m


def predict(tenant: str, case_id: str, k: int = 5, write_back: bool = True) -> dict:
    """gnn -> graph: top-k link predictions for a case; writes RELATED_TO
    edges back to FalkorDB and appends a gold-zone parquet record."""
    ds = build_dataset(tenant)
    if case_id not in ds["idx"]:
        return {"case_id": case_id, "predictions": [], "reason": "case not in graph"}
    model = _load_model()
    if model is None:
        res = train(tenant)
        if not res.get("trained"):
            return {"case_id": case_id, "predictions": [], "reason": res.get("reason")}
        model = _load_model()

    H = model.forward(ds["A"], ds["X"])
    i = ds["idx"][case_id]
    norms = np.linalg.norm(H, axis=1) + 1e-6
    scores = 2.5 * (H @ H[i]) / (norms * norms[i])  # cosine logits, as in training
    # exclude self and existing positive neighbors
    scores[i] = -np.inf
    for a, b, _w in ds["pos"]:
        if a == i:
            scores[b] = -np.inf
        if b == i:
            scores[a] = -np.inf
    top = np.argsort(-scores)[:k]
    top = [j for j in top if np.isfinite(scores[j])]  # drop self/existing edges
    probs = 1.0 / (1.0 + np.exp(-np.clip(scores[top], -30, 30)))

    preds = []
    for j, p in zip(top, probs):
        dst = ds["ids"][int(j)]
        preds.append({"case_id": dst, "score": round(float(p), 4),
                      "model_version": MODEL_VERSION})
        if write_back:
            graphdb.write_related(tenant, case_id, dst, float(p), "gnn")
    if preds:
        lakehouse_io.append_predictions([
            {"tenant": tenant, "src_case": case_id, "dst_case": p["case_id"],
             "score": p["score"], "model_version": MODEL_VERSION,
             "predicted_at": int(time.time())}
            for p in preds
        ])
    return {"case_id": case_id, "predictions": preds, "model_version": MODEL_VERSION}

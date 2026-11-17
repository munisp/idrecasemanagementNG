# Author: Patrick Munis at newwave
"""Gateway-first object storage client (stdlib only — no vendor SDKs).

The platform's storage engine is Apache OpenDAL via the storage-rs
gateway (STORAGE_GATEWAY_URL). This module returns an object exposing the
minio-client call shape the services already use (get_object/put_object/
stat_object/remove_object), backed by:

  1. the storage-rs gateway when STORAGE_GATEWAY_URL is set (all real
     deployments), or
  2. a direct MinIO client as a local-dev fallback.

Callers never name a storage vendor.
"""

from __future__ import annotations

import io
import os
import urllib.error
import urllib.request


class NotFoundError(Exception):
    pass


class _ReadResult:
    """Matches the minio get_object return shape the services use."""

    def __init__(self, data: bytes):
        self._buf = io.BytesIO(data)

    def read(self) -> bytes:
        return self._buf.read()

    def close(self) -> None:
        self._buf.close()


class _GatewayStore:
    def __init__(self, base: str, token: str):
        self.base = base.rstrip("/")
        self.token = token

    def _req(self, method: str, path: str, data: bytes | None = None,
             headers: dict | None = None):
        req = urllib.request.Request(self.base + path, data=data, method=method)
        if self.token:
            req.add_header("X-Internal-Auth", self.token)
        for k, v in (headers or {}).items():
            req.add_header(k, v)
        try:
            return urllib.request.urlopen(req, timeout=300)
        except urllib.error.HTTPError as e:
            if e.code == 404:
                raise NotFoundError(path) from e
            raise

    def get_object(self, bucket: str, key: str) -> _ReadResult:
        with self._req("GET", f"/objects/{bucket}/{key}") as resp:
            return _ReadResult(resp.read())

    def put_object(self, bucket: str, key: str, data, length: int = -1,
                   content_type: str = "", metadata: dict | None = None):
        body = data.read() if hasattr(data, "read") else bytes(data)
        headers = {"Content-Type": content_type or "application/octet-stream"}
        for k, v in (metadata or {}).items():
            headers[f"X-Object-Meta-{k}"] = str(v)
        with self._req("PUT", f"/objects/{bucket}/{key}", data=body, headers=headers):
            return None

    def stat_object(self, bucket: str, key: str):
        with self._req("HEAD", f"/objects/{bucket}/{key}") as resp:
            return {"size": int(resp.headers.get("Content-Length", "0")),
                    "etag": resp.headers.get("X-Object-Etag", "")}

    def remove_object(self, bucket: str, key: str) -> None:
        with self._req("DELETE", f"/objects/{bucket}/{key}"):
            return None


class _MinioStore:
    """Dev fallback: wraps the minio SDK (imported lazily so the gateway
    path never needs the package)."""

    def __init__(self, endpoint: str, user: str, password: str):
        from minio import Minio
        self._mc = Minio(endpoint, access_key=user, secret_key=password, secure=False)

    def get_object(self, bucket: str, key: str):
        return self._mc.get_object(bucket, key)

    def put_object(self, bucket: str, key: str, data, length: int = -1,
                   content_type: str = "", metadata: dict | None = None):
        return self._mc.put_object(bucket, key, data, length,
                                   content_type=content_type or None,
                                   metadata=metadata or None)

    def stat_object(self, bucket: str, key: str):
        st = self._mc.stat_object(bucket, key)
        return {"size": st.size, "etag": st.etag}

    def remove_object(self, bucket: str, key: str) -> None:
        self._mc.remove_object(bucket, key)


def make_object_store(endpoint: str = "", user: str = "", password: str = ""):
    """Gateway when configured (production), else MinIO dev fallback."""
    base = os.environ.get("STORAGE_GATEWAY_URL", "").strip()
    if base:
        return _GatewayStore(base, os.environ.get("STORAGE_INTERNAL_TOKEN", ""))
    return _MinioStore(endpoint or os.environ.get("MINIO_ENDPOINT", "localhost:9000"),
                       user or os.environ.get("MINIO_USER", "idre"),
                       password or os.environ.get("MINIO_PASSWORD", "idre-secret"))


__all__ = ["make_object_store", "NotFoundError"]

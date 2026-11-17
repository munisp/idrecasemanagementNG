// Author: Patrick Munis at newwave
// Object-storage abstraction for the platform.
//
// Apache OpenDAL is the platform's storage engine: all object I/O goes
// through the storage-rs gateway (services/storage-rs), which runs OpenDAL
// against whatever backend the deployment configures (MinIO/S3/GCS/Azure/
// fs) — no storage-vendor SDK semantics leak into application code. The
// minioStore below is a DEV-ONLY fallback for local hacking without the
// gateway; compose and Helm always wire STORAGE_GATEWAY_URL.
//
// The interface mirrors exactly the operations the platform uses:
// put/get/range-get/stat/remove plus multipart upload for resumable
// sharebox transfers.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// putOpts mirrors the two minio.PutObjectOptions fields the platform uses.
type putOpts struct {
	ContentType  string
	UserMetadata map[string]string
}

// objectInfo mirrors the minio.ObjectInfo fields the platform reads.
type objectInfo struct {
	Size        int64
	ETag        string
	ContentType string
}

type objectStoreAPI interface {
	PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, opts putOpts) error
	// GetObject returns an error eagerly (a HEAD round-trip) so callers keep
	// their existing "err => 404" handling instead of discovering failure
	// at first Read.
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	// GetObjectRange: endInclusive < 0 means "to end of object".
	GetObjectRange(ctx context.Context, bucket, key string, start, endInclusive int64) (io.ReadCloser, error)
	StatObject(ctx context.Context, bucket, key string) (objectInfo, error)
	RemoveObject(ctx context.Context, bucket, key string) error
	NewMultipartUpload(ctx context.Context, bucket, key string, opts putOpts) (string, error)
	PutObjectPart(ctx context.Context, bucket, key, uploadID string, partNo int, r io.Reader, size int64) (partMeta, error)
	CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []partMeta) error
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
}

// ---------------------------------------------------------------------------
// OpenDAL gateway client (primary)
// ---------------------------------------------------------------------------

type gatewayStore struct {
	base  string // e.g. http://storage:8094
	token string
	hc    *http.Client
}

func newGatewayStore(baseURL, token string) (*gatewayStore, error) {
	g := &gatewayStore{
		base:  strings.TrimRight(baseURL, "/"),
		token: token,
		hc:    &http.Client{Timeout: 10 * time.Minute}, // 100MB docs over slow links
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+"/healthz", nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("storage gateway unreachable: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("storage gateway unhealthy: HTTP %d", resp.StatusCode)
	}
	return g, nil
}

func (g *gatewayStore) do(ctx context.Context, method, path string, body io.Reader, extra map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return nil, err
	}
	if g.token != "" {
		req.Header.Set("X-Internal-Auth", g.token)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusNotFound {
			return nil, errObjectNotFound{msg: string(msg)}
		}
		return nil, fmt.Errorf("storage gateway %s %s: HTTP %d %s", method, path, resp.StatusCode, msg)
	}
	return resp, nil
}

// errObjectNotFound lets callers distinguish missing objects if needed.
type errObjectNotFound struct{ msg string }

func (e errObjectNotFound) Error() string { return "object not found: " + e.msg }

func esc(bucket, key string) string { return bucket + "/" + key }

func (g *gatewayStore) PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, opts putOpts) error {
	extra := map[string]string{}
	if opts.ContentType != "" {
		extra["Content-Type"] = opts.ContentType
	}
	for k, v := range opts.UserMetadata {
		extra["X-Object-Meta-"+k] = v
	}
	resp, err := g.do(ctx, http.MethodPut, "/objects/"+esc(bucket, key), r, extra)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (g *gatewayStore) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	// Eager HEAD keeps the minio-style "err at GetObject => 404" contract.
	if _, err := g.StatObject(ctx, bucket, key); err != nil {
		return nil, err
	}
	resp, err := g.do(ctx, http.MethodGet, "/objects/"+esc(bucket, key), nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (g *gatewayStore) GetObjectRange(ctx context.Context, bucket, key string, start, endInclusive int64) (io.ReadCloser, error) {
	spec := "bytes=" + strconv.FormatInt(start, 10) + "-"
	if endInclusive >= 0 {
		spec += strconv.FormatInt(endInclusive, 10)
	}
	resp, err := g.do(ctx, http.MethodGet, "/objects/"+esc(bucket, key), nil, map[string]string{"Range": spec})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (g *gatewayStore) StatObject(ctx context.Context, bucket, key string) (objectInfo, error) {
	resp, err := g.do(ctx, http.MethodHead, "/objects/"+esc(bucket, key), nil, nil)
	if err != nil {
		return objectInfo{}, err
	}
	defer resp.Body.Close()
	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	return objectInfo{
		Size:        size,
		ETag:        resp.Header.Get("X-Object-Etag"),
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

func (g *gatewayStore) RemoveObject(ctx context.Context, bucket, key string) error {
	resp, err := g.do(ctx, http.MethodDelete, "/objects/"+esc(bucket, key), nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (g *gatewayStore) NewMultipartUpload(ctx context.Context, bucket, key string, opts putOpts) (string, error) {
	extra := map[string]string{}
	if opts.ContentType != "" {
		extra["Content-Type"] = opts.ContentType
	}
	for k, v := range opts.UserMetadata {
		extra["X-Object-Meta-"+k] = v
	}
	resp, err := g.do(ctx, http.MethodPost, "/upload-begin/"+esc(bucket, key), nil, extra)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.UploadID == "" {
		return "", fmt.Errorf("storage gateway: bad upload_begin response")
	}
	return out.UploadID, nil
}

func (g *gatewayStore) PutObjectPart(ctx context.Context, bucket, key, uploadID string, partNo int, r io.Reader, size int64) (partMeta, error) {
	resp, err := g.do(ctx, http.MethodPut,
		fmt.Sprintf("/uploads/%s/%s/parts/%d", bucket, uploadID, partNo), r, nil)
	if err != nil {
		return partMeta{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Size int64 `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return partMeta{}, fmt.Errorf("storage gateway: bad part response")
	}
	return partMeta{N: partNo, ETag: "", PlainBytes: 0, SealedBytes: out.Size}, nil
}

func (g *gatewayStore) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []partMeta) error {
	resp, err := g.do(ctx, http.MethodPost,
		fmt.Sprintf("/uploads/%s/%s/complete", bucket, uploadID), nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (g *gatewayStore) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	resp, err := g.do(ctx, http.MethodPost,
		fmt.Sprintf("/uploads/%s/%s/abort", bucket, uploadID), nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ---------------------------------------------------------------------------
// minio-go adapter (dev fallback only)
// ---------------------------------------------------------------------------

type minioStore struct{ mc *minio.Client }

func (m *minioStore) PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, opts putOpts) error {
	_, err := m.mc.PutObject(ctx, bucket, key, r, size, minio.PutObjectOptions{
		ContentType:  opts.ContentType,
		UserMetadata: opts.UserMetadata,
	})
	return err
}

func (m *minioStore) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	if _, err := m.mc.StatObject(ctx, bucket, key, minio.StatObjectOptions{}); err != nil {
		return nil, err
	}
	return m.mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
}

func (m *minioStore) GetObjectRange(ctx context.Context, bucket, key string, start, endInclusive int64) (io.ReadCloser, error) {
	opt := minio.GetObjectOptions{}
	end := endInclusive
	if end < 0 {
		st, err := m.mc.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
		if err != nil {
			return nil, err
		}
		end = st.Size - 1
	}
	if err := opt.SetRange(start, end); err != nil {
		return nil, err
	}
	return m.mc.GetObject(ctx, bucket, key, opt)
}

func (m *minioStore) StatObject(ctx context.Context, bucket, key string) (objectInfo, error) {
	st, err := m.mc.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return objectInfo{}, err
	}
	return objectInfo{Size: st.Size, ETag: st.ETag, ContentType: st.ContentType}, nil
}

func (m *minioStore) RemoveObject(ctx context.Context, bucket, key string) error {
	return m.mc.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

func (m *minioStore) NewMultipartUpload(ctx context.Context, bucket, key string, opts putOpts) (string, error) {
	core := minio.Core{Client: m.mc}
	return core.NewMultipartUpload(ctx, bucket, key, minio.PutObjectOptions{
		ContentType:  opts.ContentType,
		UserMetadata: opts.UserMetadata,
	})
}

func (m *minioStore) PutObjectPart(ctx context.Context, bucket, key, uploadID string, partNo int, r io.Reader, size int64) (partMeta, error) {
	core := minio.Core{Client: m.mc}
	part, err := core.PutObjectPart(ctx, bucket, key, uploadID, partNo, r, size, minio.PutObjectPartOptions{})
	if err != nil {
		return partMeta{}, err
	}
	return partMeta{N: partNo, ETag: part.ETag, SealedBytes: part.Size}, nil
}

func (m *minioStore) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []partMeta) error {
	cparts := make([]minio.CompletePart, len(parts))
	for i, p := range parts {
		cparts[i] = minio.CompletePart{PartNumber: p.N, ETag: p.ETag}
	}
	core := minio.Core{Client: m.mc}
	_, err := core.CompleteMultipartUpload(ctx, bucket, key, uploadID, cparts, minio.PutObjectOptions{})
	return err
}

func (m *minioStore) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	core := minio.Core{Client: m.mc}
	return core.AbortMultipartUpload(ctx, bucket, key, uploadID)
}

// ---------------------------------------------------------------------------
// selection
// ---------------------------------------------------------------------------

func newMinioStore(endpoint, user, pass string) (*minioStore, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(user, pass, ""),
		Secure: false, // TLS terminated at ingress in prod; in-cluster plaintext
	})
	if err != nil {
		return nil, err
	}
	return &minioStore{mc: mc}, nil
}

var _ = bytes.MinRead // keep bytes import if unused after refactors

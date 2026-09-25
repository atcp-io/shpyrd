// Package objectstore talks to the platform's object store (Garage,
// RFC-0046) as its administrator: buckets, keys and permissions through the
// admin API, retention through the S3 API. Consumers never use this
// package; they get a key in a Secret that opens one bucket.
package objectstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

// Region consumers put in their configuration; Garage accepts the one it
// is configured with (s3_region).
const Region = "garage"

// Store is a connection to the object store as administrator.
type Store struct {
	Endpoint string // S3 endpoint consumers use: http://object-storage.shpyrd-system.svc:3900
	admin    string // admin API base: http://object-storage.shpyrd-system.svc:3903
	token    string
	http     *http.Client
	// platform is the key the store uses for S3-level settings (lifecycle,
	// emptying a bucket); created on first need.
	platform *Credential
}

// Connect prepares a client; nothing is contacted until the first call.
func Connect(endpoint, adminEndpoint, adminToken string) (*Store, error) {
	if endpoint == "" || adminEndpoint == "" || adminToken == "" {
		return nil, errors.New("object storage endpoint, admin endpoint and admin token are required")
	}
	return &Store{Endpoint: endpoint, admin: strings.TrimRight(adminEndpoint, "/"), token: adminToken, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// ErrNotFound is returned for unknown buckets and keys.
var ErrNotFound = errors.New("not found")

func (s *Store) call(ctx context.Context, method, path string, query url.Values, body, out interface{}) error {
	u := s.admin + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("object storage admin API: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode >= 300:
		return fmt.Errorf("object storage admin API %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// --- layout ------------------------------------------------------------------

type clusterStatus struct {
	LayoutVersion int64 `json:"layoutVersion"`
	Nodes         []struct {
		ID            string `json:"id"`
		IsUp          bool   `json:"isUp"`
		DataPartition *struct {
			Available int64 `json:"available"`
			Total     int64 `json:"total"`
		} `json:"dataPartition"`
		Role *struct {
			Capacity *int64 `json:"capacity"`
		} `json:"role"`
	} `json:"nodes"`
}

// EnsureLayout gives the single node its role on a fresh store (Garage
// stores nothing until a layout is applied): zone "shpyrd", the capacity of
// its volume.
func (s *Store) EnsureLayout(ctx context.Context, capacityBytes int64) error {
	var st clusterStatus
	if err := s.call(ctx, "GET", "/v2/GetClusterStatus", nil, nil, &st); err != nil {
		return err
	}
	if len(st.Nodes) == 0 {
		return errors.New("object storage has no node yet")
	}
	if st.LayoutVersion > 0 {
		return nil
	}
	node := st.Nodes[0]
	if !node.IsUp {
		return errors.New("object storage node is not up yet")
	}
	cap := capacityBytes
	if node.DataPartition != nil && node.DataPartition.Total > 0 && (cap == 0 || cap > node.DataPartition.Total) {
		cap = node.DataPartition.Total
	}
	if cap <= 0 {
		cap = 10 << 30
	}
	body := map[string]interface{}{"roles": []map[string]interface{}{{"id": node.ID, "zone": "shpyrd", "capacity": cap, "tags": []string{"platform"}}}}
	if err := s.call(ctx, "POST", "/v2/UpdateClusterLayout", nil, body, nil); err != nil {
		return fmt.Errorf("layout: %w", err)
	}
	if err := s.call(ctx, "POST", "/v2/ApplyClusterLayout", nil, map[string]interface{}{"version": 1}, nil); err != nil {
		return fmt.Errorf("apply layout: %w", err)
	}
	return nil
}

// --- buckets -----------------------------------------------------------------

type bucketInfo struct {
	ID            string   `json:"id"`
	GlobalAliases []string `json:"globalAliases"`
	Objects       int64    `json:"objects"`
	Bytes         int64    `json:"bytes"`
	Keys          []struct {
		AccessKeyID string `json:"accessKeyId"`
		Permissions struct {
			Read, Write, Owner bool
		} `json:"permissions"`
	} `json:"keys"`
}

// BucketSpec is what a consumer asked for.
type BucketSpec struct {
	Name          string
	Versioning    bool // Garage has no object versioning; accepted and ignored
	RetentionDays int32
}

func (s *Store) bucketByName(ctx context.Context, name string) (*bucketInfo, error) {
	var info bucketInfo
	q := url.Values{"globalAlias": {name}}
	if err := s.call(ctx, "GET", "/v2/GetBucketInfo", q, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// EnsureBucket creates the bucket when missing and applies the retention
// lifecycle.
func (s *Store) EnsureBucket(ctx context.Context, spec BucketSpec) error {
	info, err := s.bucketByName(ctx, spec.Name)
	if errors.Is(err, ErrNotFound) {
		info = &bucketInfo{}
		if err := s.call(ctx, "POST", "/v2/CreateBucket", nil, map[string]interface{}{"globalAlias": spec.Name}, info); err != nil {
			return fmt.Errorf("create bucket %s: %w", spec.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("bucket %s: %w", spec.Name, err)
	}
	s3, err := s.platformS3(ctx, info.ID)
	if err != nil {
		return err
	}
	cfg := lifecycle.NewConfiguration()
	if spec.RetentionDays > 0 {
		cfg.Rules = []lifecycle.Rule{{ID: "shpyrd-retention", Status: "Enabled", Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(spec.RetentionDays)}}}
	}
	if err := s3.SetBucketLifecycle(ctx, spec.Name, cfg); err != nil {
		return fmt.Errorf("retention on %s: %w", spec.Name, err)
	}
	return nil
}

// DeleteBucket removes every object and the bucket.
func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	info, err := s.bucketByName(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	s3, err := s.platformS3(ctx, info.ID)
	if err != nil {
		return err
	}
	objects := s3.ListObjects(ctx, name, minio.ListObjectsOptions{Recursive: true})
	for e := range s3.RemoveObjects(ctx, name, objects, minio.RemoveObjectsOptions{}) {
		if e.Err != nil {
			return fmt.Errorf("empty bucket %s: %w", name, e.Err)
		}
	}
	return s.call(ctx, "POST", "/v2/DeleteBucket", url.Values{"id": {info.ID}}, nil, nil)
}

// --- keys --------------------------------------------------------------------

// Credential is a key scoped to one bucket.
type Credential struct {
	AccessKey string
	SecretKey string
}

type keyInfo struct {
	AccessKeyID     string  `json:"accessKeyId"`
	SecretAccessKey *string `json:"secretAccessKey"`
	Name            string  `json:"name"`
}

// EnsureUser makes sure a key exists with read and write on bucket, and
// nothing else. With an access key pair given (from the consumer's Secret)
// the same pair is kept, re-imported if the store lost it; otherwise the
// store generates one.
func (s *Store) EnsureUser(ctx context.Context, bucket, accessKey, secretKey string) (Credential, error) {
	info, err := s.bucketByName(ctx, bucket)
	if err != nil {
		return Credential{}, fmt.Errorf("bucket %s: %w", bucket, err)
	}
	name := bucket // the key is named after the bucket it opens
	var key keyInfo
	switch {
	case accessKey != "" && secretKey != "":
		err = s.call(ctx, "GET", "/v2/GetKeyInfo", url.Values{"id": {accessKey}}, nil, &key)
		if errors.Is(err, ErrNotFound) {
			err = s.call(ctx, "POST", "/v2/ImportKey", nil, map[string]interface{}{"accessKeyId": accessKey, "secretAccessKey": secretKey, "name": name}, &key)
		}
		if err != nil {
			return Credential{}, fmt.Errorf("key for %s: %w", bucket, err)
		}
		key.SecretAccessKey = &secretKey
	default:
		if err := s.call(ctx, "POST", "/v2/CreateKey", nil, map[string]interface{}{"name": name, "neverExpires": true}, &key); err != nil {
			return Credential{}, fmt.Errorf("create key for %s: %w", bucket, err)
		}
	}
	perm := map[string]interface{}{"bucketId": info.ID, "accessKeyId": key.AccessKeyID, "permissions": map[string]bool{"read": true, "write": true, "owner": false}}
	if err := s.call(ctx, "POST", "/v2/AllowBucketKey", nil, perm, nil); err != nil {
		return Credential{}, fmt.Errorf("allow key on %s: %w", bucket, err)
	}
	secret := secretKey
	if key.SecretAccessKey != nil && *key.SecretAccessKey != "" {
		secret = *key.SecretAccessKey
	}
	return Credential{AccessKey: key.AccessKeyID, SecretKey: secret}, nil
}

// DeleteUser removes the key.
func (s *Store) DeleteUser(ctx context.Context, _ string, accessKey string) error {
	if accessKey == "" {
		return nil
	}
	err := s.call(ctx, "POST", "/v2/DeleteKey", url.Values{"id": {accessKey}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// --- usage -------------------------------------------------------------------

// Usage is what a bucket holds.
type Usage struct {
	Bytes   int64
	Objects int64
}

// Capacity is the store's disk and what each bucket holds.
type Capacity struct {
	TotalBytes int64
	UsedBytes  int64
	Buckets    map[string]Usage
	MeasuredAt time.Time
}

type bucketListEntry struct {
	ID            string   `json:"id"`
	GlobalAliases []string `json:"globalAliases"`
}

// Usage reports per-bucket usage and the disk behind the store.
func (s *Store) Usage(ctx context.Context) (*Capacity, error) {
	out := &Capacity{Buckets: map[string]Usage{}, MeasuredAt: time.Now()}
	var list []bucketListEntry
	if err := s.call(ctx, "GET", "/v2/ListBuckets", nil, nil, &list); err != nil {
		return nil, err
	}
	for _, b := range list {
		var info bucketInfo
		if err := s.call(ctx, "GET", "/v2/GetBucketInfo", url.Values{"id": {b.ID}}, nil, &info); err != nil {
			continue
		}
		for _, alias := range info.GlobalAliases {
			out.Buckets[alias] = Usage{Bytes: info.Bytes, Objects: info.Objects}
		}
	}
	var st clusterStatus
	if err := s.call(ctx, "GET", "/v2/GetClusterStatus", nil, nil, &st); err == nil {
		for _, n := range st.Nodes {
			if n.DataPartition != nil {
				out.TotalBytes += n.DataPartition.Total
				out.UsedBytes += n.DataPartition.Total - n.DataPartition.Available
			}
		}
	}
	return out, nil
}

// Ping checks the admin API answers.
func (s *Store) Ping(ctx context.Context) error {
	return s.call(ctx, "GET", "/v2/GetClusterStatus", nil, nil, &clusterStatus{})
}

// --- the platform's own key ----------------------------------------------------

// platformS3 returns an S3 client holding a key that owns bucketID, for the
// settings the admin API does not expose (lifecycle) and for emptying a
// bucket. One key, named shpyrd-platform, reused across buckets.
func (s *Store) platformS3(ctx context.Context, bucketID string) (*minio.Client, error) {
	if s.platform == nil {
		// ListKeys entries carry the access key as "id".
		var keys []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := s.call(ctx, "GET", "/v2/ListKeys", nil, nil, &keys); err != nil {
			return nil, err
		}
		found := ""
		for _, k := range keys {
			if k.Name == "shpyrd-platform" {
				found = k.ID
				break
			}
		}
		var key keyInfo
		if found != "" {
			if err := s.call(ctx, "GET", "/v2/GetKeyInfo", url.Values{"id": {found}, "showSecretKey": {"true"}}, nil, &key); err != nil {
				return nil, err
			}
		} else if err := s.call(ctx, "POST", "/v2/CreateKey", nil, map[string]interface{}{"name": "shpyrd-platform", "neverExpires": true}, &key); err != nil {
			return nil, fmt.Errorf("platform key: %w", err)
		}
		if key.SecretAccessKey == nil {
			return nil, errors.New("platform key has no secret")
		}
		s.platform = &Credential{AccessKey: key.AccessKeyID, SecretKey: *key.SecretAccessKey}
	}
	perm := map[string]interface{}{"bucketId": bucketID, "accessKeyId": s.platform.AccessKey, "permissions": map[string]bool{"read": true, "write": true, "owner": true}}
	if err := s.call(ctx, "POST", "/v2/AllowBucketKey", nil, perm, nil); err != nil {
		return nil, fmt.Errorf("platform key on bucket: %w", err)
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return nil, err
	}
	return minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(s.platform.AccessKey, s.platform.SecretKey, ""), Secure: u.Scheme == "https", Region: Region})
}

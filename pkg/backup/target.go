package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Target is where archives go: an S3-compatible bucket outside the
// cluster (the provider's object storage), so backups survive it.
type Target struct {
	Bucket   string // from s3://bucket/prefix
	Prefix   string
	Endpoint string // https://s3.us-east-1.amazonaws.com, https://<ns>.compat.objectstorage.<region>.oraclecloud.com
	Region   string
	// Static credentials; empty falls back to the environment and the
	// container credentials (EKS Pod Identity), then ~/.aws/credentials.
	AccessKey string
	SecretKey string
	// Profile in ~/.aws/credentials when the keys are empty (operator side).
	Profile string
}

// ParseURI splits s3://bucket/prefix.
func ParseURI(uri string) (bucket, prefix string, err error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return "", "", fmt.Errorf("backup target %q: use s3://bucket[/prefix]", uri)
	}
	return u.Host, strings.Trim(u.Path, "/"), nil
}

// FromEnv reads a target from the environment (the job's Secret).
func FromEnv() (*Target, error) {
	uri := os.Getenv("SHPYRD_BACKUP_TARGET")
	if uri == "" {
		return nil, errors.New("SHPYRD_BACKUP_TARGET is not set")
	}
	bucket, prefix, err := ParseURI(uri)
	if err != nil {
		return nil, err
	}
	return &Target{
		Bucket: bucket, Prefix: prefix,
		Endpoint:  os.Getenv("SHPYRD_BACKUP_ENDPOINT"),
		Region:    os.Getenv("SHPYRD_BACKUP_REGION"),
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}, nil
}

func (t *Target) client() (*minio.Client, error) {
	endpoint := t.Endpoint
	if endpoint == "" {
		region := t.Region
		if region == "" {
			region = "us-east-1"
		}
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("backup endpoint: %w", err)
	}
	var creds *credentials.Credentials
	switch {
	case t.AccessKey != "" && t.SecretKey != "":
		creds = credentials.NewStaticV4(t.AccessKey, t.SecretKey, "")
	default:
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.IAM{}, // container credentials (EKS Pod Identity), instance roles
			&credentials.FileAWSCredentials{Profile: t.Profile},
		})
	}
	return minio.New(u.Host, &minio.Options{Creds: creds, Secure: u.Scheme == "https", Region: t.Region})
}

// Name of an archive: <prefix>/<cluster>-<timestamp>.tar.gz.age.
func (t *Target) key(name string) string {
	if t.Prefix == "" {
		return name
	}
	return t.Prefix + "/" + name
}

// Upload stores an archive.
func (t *Target) Upload(ctx context.Context, name string, r io.Reader, size int64) error {
	c, err := t.client()
	if err != nil {
		return err
	}
	_, err = c.PutObject(ctx, t.Bucket, t.key(name), r, size, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("upload %s: %w", name, err)
	}
	return nil
}

// Entry is one stored archive.
type Entry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// List returns the archives, newest first.
func (t *Target) List(ctx context.Context) ([]Entry, error) {
	c, err := t.client()
	if err != nil {
		return nil, err
	}
	prefix := ""
	if t.Prefix != "" {
		prefix = t.Prefix + "/"
	}
	var out []Entry
	for obj := range c.ListObjects(ctx, t.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: false}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list %s: %w", t.Bucket, obj.Err)
		}
		name := strings.TrimPrefix(obj.Key, prefix)
		if !strings.HasSuffix(name, ".tar.gz.age") {
			continue
		}
		out = append(out, Entry{Name: name, Size: obj.Size, Modified: obj.LastModified})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// Download opens an archive.
func (t *Target) Download(ctx context.Context, name string) (io.ReadCloser, error) {
	c, err := t.client()
	if err != nil {
		return nil, err
	}
	obj, err := c.GetObject(ctx, t.Bucket, t.key(name), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := obj.Stat(); err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	return obj, nil
}

// Prune keeps the newest keep archives and deletes the rest.
func (t *Target) Prune(ctx context.Context, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	entries, err := t.List(ctx)
	if err != nil {
		return 0, err
	}
	c, err := t.client()
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, e := range entries[min(keep, len(entries)):] {
		if err := c.RemoveObject(ctx, t.Bucket, t.key(e.Name), minio.RemoveObjectOptions{}); err != nil {
			return deleted, fmt.Errorf("prune %s: %w", e.Name, err)
		}
		deleted++
	}
	return deleted, nil
}

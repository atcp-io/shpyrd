package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/gin-gonic/gin"
)

// maxSourceSize bounds uploaded source archives.
const maxSourceSize = 512 << 20

var sourceName = regexp.MustCompile(`^[a-f0-9]{64}\.tgz$`)

// SourceStore keeps uploaded source archives on disk, addressed by their
// SHA-256. kpack fetches them from the cluster-internal URL as a blob source.
type SourceStore struct {
	Dir string
	// BaseURL is the cluster-internal address of this server, e.g.
	// http://shpyrd-server.shpyrd-system.svc.
	BaseURL string
}

// SourceInfo is returned after an upload.
type SourceInfo struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	URL    string `json:"url"`
}

func (s *SourceStore) fileName(sha string) string { return sha + ".tgz" }

func (s *SourceStore) url(sha string) string {
	return s.BaseURL + "/api/sources/" + s.fileName(sha)
}

// Put streams an archive to disk and returns its identity. Duplicate
// uploads are deduplicated by hash.
func (s *SourceStore) Put(r io.Reader) (*SourceInfo, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(s.Dir, "upload-*.part")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, maxSourceSize+1))
	if err != nil {
		return nil, err
	}
	if n > maxSourceSize {
		return nil, fmt.Errorf("archive exceeds %d MiB", maxSourceSize>>20)
	}
	if n == 0 {
		return nil, errors.New("empty archive")
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	sha := hex.EncodeToString(h.Sum(nil))
	final := filepath.Join(s.Dir, s.fileName(sha))
	if _, err := os.Stat(final); err == nil {
		return &SourceInfo{SHA256: sha, Size: n, URL: s.url(sha)}, nil
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return nil, err
	}
	return &SourceInfo{SHA256: sha, Size: n, URL: s.url(sha)}, nil
}

func (s *Server) uploadSource(c *gin.Context) {
	if s.sources == nil {
		abort(c, http.StatusServiceUnavailable, errors.New("source store not configured"))
		return
	}
	info, err := s.sources.Put(c.Request.Body)
	if err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	s.log.Info("source uploaded", "sha256", info.SHA256[:12], "size", info.Size)
	c.JSON(http.StatusCreated, info)
}

func (s *Server) serveSource(c *gin.Context) {
	name := c.Param("name")
	if s.sources == nil || !sourceName.MatchString(name) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	path := filepath.Join(s.sources.Dir, name)
	if _, err := os.Stat(path); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.Header("Content-Type", "application/gzip")
	c.File(path)
}

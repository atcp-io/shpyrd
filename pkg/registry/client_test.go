package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSplitReference(t *testing.T) {
	cases := []struct{ ref, host, repo, digest string }{
		{"10.96.0.50:5000/apps/shop@sha256:abc", "10.96.0.50:5000", "apps/shop", "sha256:abc"},
		{"10.96.0.50:5000/apps/shop:b3", "10.96.0.50:5000", "apps/shop", ""},
		{"gru.ocir.io/ns/apps/shop:b3@sha256:def", "gru.ocir.io", "ns/apps/shop", "sha256:def"},
		{"registry.example.com/shpyrd/builder", "registry.example.com", "shpyrd/builder", ""},
	}
	for _, tc := range cases {
		h, r, d := SplitReference(tc.ref)
		if h != tc.host || r != tc.repo || d != tc.digest {
			t.Errorf("%s -> %s %s %s", tc.ref, h, r, d)
		}
	}
}

func TestFromDockerConfig(t *testing.T) {
	cfg := `{"auths":{"10.96.0.50:5000":{"auth":"c2hweXJkOnNlY3JldA=="},"other":{"username":"u","password":"p"}}}`
	c := FromDockerConfig("10.96.0.50:5000", []byte(cfg))
	if c.Username != "shpyrd" || c.Password != "secret" {
		t.Errorf("credential from auth field: %s/%s", c.Username, c.Password)
	}
	if c := FromDockerConfig("missing", []byte(cfg)); c.Username != "" {
		t.Error("no credential for an unknown host")
	}
}

func TestCatalogTagsDelete(t *testing.T) {
	var deleted []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "shpyrd" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v2/_catalog" && r.URL.Query().Get("last") == "":
			w.Header().Set("Link", `</v2/_catalog?n=2&last=apps%2Fshop>; rel="next"`)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"repositories": []string{"apps/api", "apps/shop"}})
		case r.URL.Path == "/v2/_catalog":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"repositories": []string{"shpyrd/builder"}})
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"tags": []string{"b1", "b2", "latest"}})
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	c := &Client{Host: host, Username: "shpyrd", Password: "pw", HTTP: srv.Client()}
	ctx := context.Background()
	repos, err := c.Catalog(ctx, 100)
	if err != nil || len(repos) != 3 || repos[2] != "shpyrd/builder" {
		t.Fatalf("catalog = %v, %v", repos, err)
	}
	tags, err := c.Tags(ctx, "apps/shop")
	if err != nil || len(tags) != 3 {
		t.Fatalf("tags = %v, %v", tags, err)
	}
	if err := c.DeleteManifest(ctx, "apps/shop", "sha256:abc"); err != nil || len(deleted) != 1 || deleted[0] != "/v2/apps/shop/manifests/sha256:abc" {
		t.Errorf("delete: %v %v", deleted, err)
	}
	bad := &Client{Host: host, Username: "x", Password: "y", HTTP: srv.Client()}
	if err := bad.Ping(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad credential: %v", err)
	}
}

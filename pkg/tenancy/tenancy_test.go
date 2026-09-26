package tenancy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shpyrd-io/shpyrd/pkg/store"
)

func TestHost(t *testing.T) {
	for in, want := range map[string]string{
		"Shpyrd.Example.com:8443": "shpyrd.example.com",
		"shop.example.com":        "shop.example.com",
		"[::1]:8443":              "::1",
		"127.0.0.1:8080":          "127.0.0.1",
		"example.com.":            "example.com",
		"":                        "",
	} {
		if got := Host(in); got != want {
			t.Errorf("Host(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInternal(t *testing.T) {
	for _, h := range []string{"localhost", "10.0.0.5", "::1", "shpyrd-server.shpyrd-system.svc.cluster.local", "shpyrd-server.shpyrd-system.svc", ""} {
		if !Internal(h) {
			t.Errorf("Internal(%q) = false", h)
		}
	}
	for _, h := range []string{"shop.example.com", "acme.shpyrd.app", "svc.example.com"} {
		if Internal(h) {
			t.Errorf("Internal(%q) = true", h)
		}
	}
}

func TestSingle(t *testing.T) {
	st := store.NewMemory()
	r := &Single{Store: st}
	for _, h := range []string{"shpyrd.example.com:8443", "anything.at.all", "10.0.0.1", ""} {
		ws, err := r.Resolve(context.Background(), h)
		if err != nil || !ws.Implicit() {
			t.Errorf("Single(%q) = %+v %v", h, ws, err)
		}
	}
}

func TestByAddress(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Slug: "acme", Name: "Acme", Address: "acme.shpyrd.app"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Slug: "intranet", Name: "Acme intranet", Address: "intranet.acme.com"}); err != nil {
		t.Fatal(err)
	}
	r := &ByAddress{Store: st, Domain: "shpyrd.app", DashboardHost: "console.shpyrd.io", TTL: time.Minute}

	cases := map[string]string{
		// workspace addresses, exact and one label under
		"acme.shpyrd.app":        "acme",
		"ACME.shpyrd.app:443":    "acme",
		"shop.acme.shpyrd.app":   "acme",
		"intranet.acme.com":      "intranet",
		"wiki.intranet.acme.com": "intranet",
		// the platform's own names
		"shpyrd.app":             store.DefaultWorkspace,
		"hello.shpyrd.app":       store.DefaultWorkspace,
		"console.shpyrd.io":      store.DefaultWorkspace,
		"CONSOLE.shpyrd.io:8443": store.DefaultWorkspace,
		"localhost:8080":         store.DefaultWorkspace,
		"10.244.0.7":             store.DefaultWorkspace,
		"shpyrd-server.shpyrd-system.svc.cluster.local": store.DefaultWorkspace,
	}
	for host, want := range cases {
		ws, err := r.Resolve(ctx, host)
		if err != nil || ws.Slug != want {
			t.Errorf("Resolve(%q) = %v %v, want %s", host, ws, err, want)
		}
	}
	for _, host := range []string{"acme.com", "deep.shop.acme.shpyrd.app", "evil.example.com", "shpyrd.io", "app.shpyrd.io"} {
		if _, err := r.Resolve(ctx, host); !errors.Is(err, ErrUnknownHost) {
			t.Errorf("Resolve(%q) = %v, want ErrUnknownHost", host, err)
		}
	}

	// Lookups are cached for the TTL, and Forget drops one.
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Slug: "beta", Name: "Beta", Address: "evil.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, "evil.example.com"); !errors.Is(err, ErrUnknownHost) {
		t.Errorf("negative result not cached: %v", err)
	}
	r.Forget("evil.example.com")
	if ws, err := r.Resolve(ctx, "evil.example.com"); err != nil || ws.Slug != "beta" {
		t.Errorf("after Forget: %v %v", ws, err)
	}
}

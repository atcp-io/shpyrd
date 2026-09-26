// Package tenancy maps the host a request arrived at to the workspace that
// answers there (RFC-0033 phase 6). The open-source platform has one
// implicit workspace and resolves every host to it; the cloud layer plugs
// in a resolver that knows many. The interface is the seam; both
// implementations live here so they are tested together.
package tenancy

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"shpyrd/pkg/store"
)

// ErrUnknownHost says no workspace answers at the host.
var ErrUnknownHost = errors.New("no workspace answers at this host")

// Resolver finds the workspace for a request host. Implementations must be
// safe for concurrent use and cheap: they run on every request.
type Resolver interface {
	Resolve(ctx context.Context, host string) (*store.Workspace, error)
}

// Host strips the port and lowercases a Host header value.
func Host(hostport string) string {
	h := strings.ToLower(strings.TrimSpace(hostport))
	if h == "" {
		return ""
	}
	if strings.HasPrefix(h, "[") { // [::1]:8443
		if i := strings.LastIndex(h, "]"); i > 0 {
			return h[1:i]
		}
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.TrimSuffix(h, ".")
}

// Internal reports whether a host is one in-cluster callers use rather
// than a public name: an IP literal, localhost, or a Kubernetes service
// name. Such requests belong to the operator, hence to the implicit
// workspace.
func Internal(host string) bool {
	switch {
	case host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost"):
		return true
	case net.ParseIP(host) != nil:
		return true
	case strings.HasSuffix(host, ".svc") || strings.Contains(host, ".svc."):
		return true
	}
	return false
}

// Single is the open-source resolver: every host is the implicit
// workspace. The row is read once; the implicit workspace is never
// renamed by slug nor suspended.
type Single struct {
	Store store.Store

	mu sync.Mutex
	ws *store.Workspace
}

// Resolve returns the implicit workspace whatever the host.
func (r *Single) Resolve(ctx context.Context, _ string) (*store.Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ws != nil {
		return r.ws, nil
	}
	ws, err := r.Store.Workspace(ctx, store.DefaultWorkspace)
	if err != nil {
		return nil, err
	}
	r.ws = ws
	return ws, nil
}

// ByAddress resolves hosts against workspace addresses: a host equal to a
// workspace's Address, or one label under it (<app>.<address>), is that
// workspace. The platform's own names (Domain, one label under it, the
// dashboard host) and internal hosts are the implicit workspace. Anything
// else is ErrUnknownHost, never a guess: a request at a host nobody claimed
// must not land in someone's workspace.
type ByAddress struct {
	Store store.Store
	// Domain is the platform domain: apps of the implicit workspace live at
	// <app>.<Domain>.
	Domain string
	// DashboardHost is where the implicit workspace's dashboard answers
	// (shpyrd.<Domain> by default).
	DashboardHost string
	// TTL bounds how long a lookup, found or not, is remembered. Zero means
	// ten seconds.
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	ws      *store.Workspace
	err     error
	expires time.Time
}

func (r *ByAddress) ttl() time.Duration {
	if r.TTL <= 0 {
		return 10 * time.Second
	}
	return r.TTL
}

// Resolve implements Resolver.
func (r *ByAddress) Resolve(ctx context.Context, hostport string) (*store.Workspace, error) {
	host := Host(hostport)
	now := time.Now()
	r.mu.Lock()
	if e, ok := r.cache[host]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.ws, e.err
	}
	r.mu.Unlock()

	ws, err := r.lookup(ctx, host)
	if err != nil && !errors.Is(err, ErrUnknownHost) {
		return nil, err // store trouble is not cached
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]entry{}
	}
	if len(r.cache) > 4096 { // a flood of unknown hosts must not grow this without bound
		r.cache = map[string]entry{}
	}
	r.cache[host] = entry{ws: ws, err: err, expires: now.Add(r.ttl())}
	r.mu.Unlock()
	return ws, err
}

func (r *ByAddress) lookup(ctx context.Context, host string) (*store.Workspace, error) {
	// Workspace addresses win over the platform domain: acme.shpyrd.app is a
	// workspace even though it also looks like <app>.<Domain>.
	if ws, err := r.Store.WorkspaceByAddress(ctx, host); err == nil {
		return ws, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if _, parent, ok := strings.Cut(host, "."); ok && parent != "" {
		if ws, err := r.Store.WorkspaceByAddress(ctx, parent); err == nil {
			return ws, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	domain := strings.ToLower(r.Domain)
	dashboard := strings.ToLower(Host(r.DashboardHost))
	platform := Internal(host) ||
		(domain != "" && (host == domain || oneLabelUnder(host, domain))) ||
		(dashboard != "" && host == dashboard)
	if !platform {
		return nil, ErrUnknownHost
	}
	return r.Store.Workspace(ctx, store.DefaultWorkspace)
}

// Forget drops a cached host, for callers that just changed an address.
func (r *ByAddress) Forget(host string) {
	r.mu.Lock()
	delete(r.cache, Host(host))
	r.mu.Unlock()
}

// oneLabelUnder reports whether host is <label>.<domain>, exactly one
// level down: where apps of a workspace live and what its wildcard
// certificate covers.
func oneLabelUnder(host, domain string) bool {
	label, ok := strings.CutSuffix(host, "."+domain)
	return ok && label != "" && !strings.Contains(label, ".")
}

// Addresses answers a workspace's address by slug with a short cache, for
// controllers that build hosts on every reconcile and have no request
// context to speak of.
type Addresses struct {
	Store store.Store
	TTL   time.Duration

	mu    sync.Mutex
	cache map[string]addrEntry
}

type addrEntry struct {
	address string
	expires time.Time
}

// Address is the address of a workspace, "" for the implicit one or an
// unknown slug. Safe for concurrent use.
func (a *Addresses) Address(slug string) string {
	if slug == "" || slug == store.DefaultWorkspace {
		return ""
	}
	ttl := a.TTL
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	now := time.Now()
	a.mu.Lock()
	if e, ok := a.cache[slug]; ok && now.Before(e.expires) {
		a.mu.Unlock()
		return e.address
	}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	address := ""
	ws, err := a.Store.Workspace(ctx, slug)
	if err == nil {
		address = ws.Address
	} else if !errors.Is(err, store.ErrNotFound) {
		return "" // store trouble: fall back to the platform domain, do not cache
	}
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[string]addrEntry{}
	}
	a.cache[slug] = addrEntry{address: address, expires: now.Add(ttl)}
	a.mu.Unlock()
	return address
}

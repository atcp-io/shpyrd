// Package ext defines how optional platform capabilities plug into shpyrd
// (RFC-0002). An extension is a Go package compiled into the binaries and
// switched on per cluster; it contributes an installer component, resource
// types with reconcilers, API routes and CLI commands through the small
// interfaces below. The registry of built-in extensions lives in ext/all.
package ext

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"shpyrd/pkg/kube"
)

// Extension is one optional capability.
type Extension interface {
	// Name is the identifier used to enable it: "auth-local".
	Name() string
	Description() string
	// Components names the installer components the extension adds
	// (directories under deploy/components) with the runlevels they join,
	// in install order; empty when the extension has no cluster component.
	Components() []ComponentRef
	// Register adds reconcilers to the controller manager (may be a no-op).
	Register(mgr ctrl.Manager, deps Deps) error
	// Routes mounts API routes. Public routes need no authentication;
	// protected ones sit behind the server's authentication.
	Routes(r Router, deps Deps) error
	// CLI returns extra top-level commands.
	CLI(g CLIGlobals) []*cobra.Command
	// Types lists the resource kinds the extension owns; disabling refuses
	// while such resources exist.
	Types() []ResourceType
}

// ComponentRef points at an installer component by name.
type ComponentRef struct {
	Name     string
	Runlevel string
}

// ResourceType describes a resource kind for the dashboard and CLI.
type ResourceType struct {
	// Kind is the CRD kind ("Postgres"); Group/Version/Resource locate it.
	Kind     string
	Group    string
	Version  string
	Resource string
	// Bindable resources can be attached to an App (RFC-0003).
	Bindable bool
}

// Router gives extensions the route groups of the API: public (no
// session), protected (any signed-in identity) and admin (platform
// administrators only, the cluster.admin action of RFC-0008).
type Router interface {
	Public() gin.IRouter
	Protected() gin.IRouter
	Admin() gin.IRouter
}

// Deps is what the server hands to extensions.
type Deps struct {
	Kube *kube.Client
	// Client is a controller-runtime client (cached when running with the
	// manager).
	Client client.Client
	// SystemNamespace holds shpyrd's own objects (shpyrd-system).
	SystemNamespace string
	// Vars are the install variables (SHPYRD_DOMAIN, SHPYRD_AUTH_URL, ...)
	// as the server sees them from its environment.
	Vars func(name string) string
	// Auth registers login providers with the server (RFC-0007).
	Auth AuthRegistry
}

// Var reads an install variable, "" when none is configured.
func (d Deps) Var(name string) string {
	if d.Vars == nil {
		return ""
	}
	return d.Vars(name)
}

// AuthRegistry is implemented by the server's relying party.
type AuthRegistry interface {
	// AddOIDC registers an OpenID Connect provider users can sign in with.
	AddOIDC(ctx context.Context, p OIDCProvider) error
	// RemoveOIDC unregisters a provider; existing sessions stay.
	RemoveOIDC(id string)
}

// OIDCProvider configures one OpenID Connect issuer.
type OIDCProvider struct {
	// ID is used in URLs (/api/auth/login?provider=<id>).
	ID string
	// Label is shown on the login page ("Email and password").
	Label string
	// Issuer is the external issuer URL, as browsers see it.
	Issuer string
	// ClientID and ClientSecret identify the dashboard at the issuer.
	ClientID     string
	ClientSecret string
	// Scopes beyond openid, email and profile.
	Scopes []string
	// Password says the issuer accepts the OAuth2 password grant for this
	// client: the dashboard shows an email/password form for the provider
	// instead of a button and exchanges the credentials itself (RFC-0012).
	Password bool
	// Kind picks the button's icon: "oidc" (default), "github", "google".
	Kind string
	// ConnectorID preselects a Dex connector (connector_id in the
	// authorization request) so Dex's chooser is skipped (RFC-0058).
	ConnectorID string
}

// CLIGlobals gives extension commands access to the CLI's connection flags.
type CLIGlobals interface {
	Kubeconfig() string
	Context() string
}

// Names returns the names of extensions, in order.
func Names(list []Extension) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.Name())
	}
	return out
}

// Find returns the extension with the name, or nil.
func Find(list []Extension, name string) Extension {
	for _, e := range list {
		if e.Name() == name {
			return e
		}
	}
	return nil
}

// Identity is a signed-in user as seen by the server (RFC-0007).
type Identity struct {
	// Subject is the provider's stable id; "admin-token" for the token.
	Subject string   `json:"subject"`
	Email   string   `json:"email,omitempty"`
	Name    string   `json:"name,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	// Provider is the login provider id ("local", "okta"); "token" for the
	// admin token.
	Provider string `json:"provider"`
	// Admin is true for the admin token and, until roles arrive (RFC-0008),
	// for every signed-in user.
	Admin bool `json:"admin"`
}

// identityKey is where the auth middleware stores the Identity.
const identityKey = "shpyrd.identity"

// SetIdentity stores the request's identity on the context.
func SetIdentity(c *gin.Context, id Identity) { c.Set(identityKey, id) }

// IdentityFrom returns the request's identity, when authenticated.
func IdentityFrom(c *gin.Context) (Identity, bool) {
	v, ok := c.Get(identityKey)
	if !ok {
		return Identity{}, false
	}
	id, ok := v.(Identity)
	return id, ok
}

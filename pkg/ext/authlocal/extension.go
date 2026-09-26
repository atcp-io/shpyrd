// Package authlocal is the auth-local extension (RFC-0007 step 3.1): a Dex
// issuer with local email/password accounts, registered as a login
// provider of the dashboard, plus account management for admins.
package authlocal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/install"
)

// Name of the extension.
const Name = "auth-local"

// ProviderID is the login provider id ("/api/auth/login?provider=local").
const ProviderID = "local"

type extension struct{}

// New returns the extension.
func New() ext.Extension { return extension{} }

func (extension) Name() string { return Name }
func (extension) Description() string {
	return "Sign in with email and password: a bundled Dex issuer stores local accounts (shpyrd users add)"
}
func (extension) Components() []ext.ComponentRef {
	return []ext.ComponentRef{{Name: "dex", Runlevel: "rc3"}}
}
func (extension) Register(ctrl.Manager, ext.Deps) error { return nil }
func (extension) Types() []ext.ResourceType             { return nil }

// Routes registers the Dex issuer as a login provider and mounts the users API.
func (extension) Routes(r ext.Router, deps ext.Deps) error {
	issuer := deps.Vars(install.VarAuthURL)
	if issuer == "" {
		return errors.New("SHPYRD_AUTH_URL is not set")
	}
	if deps.Auth != nil && deps.Kube != nil {
		go registerProvider(context.Background(), deps, issuer)
	}
	store := &Store{Namespace: deps.SystemNamespace}
	if deps.Kube != nil {
		store.Dynamic = deps.Kube.Dynamic
	}
	// Accounts are managed by platform administrators (RFC-0007, RFC-0008).
	h := &handlers{store: store}
	api := r.Admin()
	api.GET("/users", h.list)
	api.POST("/users", h.create)
	api.PUT("/users/:email/password", h.setPassword)
	api.DELETE("/users/:email", h.delete)
	// Login methods (RFC-0058 connectors) managed from the Workspace page.
	ch := &connectorHandlers{deps: deps, issuer: issuer}
	api.GET("/auth/connectors", ch.list)
	api.POST("/auth/connectors", ch.add)
	api.DELETE("/auth/connectors/:id", ch.remove)
	return nil
}

// registerProvider reads the client secret and registers Dex with the
// relying party, retrying while Dex is still starting (it may come up in
// the same runlevel as the server).
func registerProvider(ctx context.Context, deps ext.Deps, issuer string) {
	delay := 2 * time.Second
	for attempt := 1; ; attempt++ {
		sec, err := deps.Kube.Kube.CoreV1().Secrets(deps.SystemNamespace).Get(ctx, install.OIDCClientSecretName, metav1.GetOptions{})
		if err == nil {
			clientID, secret := strings.TrimSpace(string(sec.Data["client-id"])), strings.TrimSpace(string(sec.Data["client-secret"]))
			err = deps.Auth.AddOIDC(ctx, ext.OIDCProvider{
				ID: ProviderID, Label: "Email and password", Issuer: issuer, Password: true,
				ClientID: clientID, ClientSecret: secret,
			})
			if err == nil {
				// One button per Dex connector (GitHub, Google; RFC-0058):
				// same issuer and client, Dex sent straight to the connector.
				err = registerConnectors(ctx, deps, issuer, clientID, secret)
			}
		}
		if err == nil {
			return
		}
		if attempt == 1 || attempt%10 == 0 {
			fmt.Printf("auth-local: login provider not ready yet (%v); retrying\n", err)
		}
		if delay < 30*time.Second {
			delay *= 2
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// ---- users API ---------------------------------------------------------------

type handlers struct{ store *Store }

type createUserRequest struct {
	Email    string `json:"email" binding:"required"`
	Name     string `json:"name"`
	Password string `json:"password" binding:"required"`
}

type passwordRequest struct {
	Password string `json:"password" binding:"required"`
}

func (h *handlers) list(c *gin.Context) {
	users, err := h.store.List(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, users)
}

func (h *handlers) create(c *gin.Context) {
	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u, err := h.store.Create(c.Request.Context(), req.Email, req.Name, req.Password)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, u)
}

func (h *handlers) setPassword(c *gin.Context) {
	var req passwordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.SetPassword(c.Request.Context(), c.Param("email"), req.Password); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *handlers) delete(c *gin.Context) {
	if id, ok := ext.IdentityFrom(c); ok && strings.EqualFold(id.Email, c.Param("email")) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "you cannot delete the account you are signed in with"})
		return
	}
	if err := h.store.Delete(c.Request.Context(), c.Param("email")); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// fail maps store errors to status codes.
func fail(c *gin.Context, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrNotEnabled):
		status = http.StatusServiceUnavailable
	case strings.Contains(err.Error(), "not found"):
		status = http.StatusNotFound
	case strings.Contains(err.Error(), "already exists"):
		status = http.StatusConflict
	}
	c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
}

// CLI returns `shpyrd users`.
func (extension) CLI(g ext.CLIGlobals) []*cobra.Command {
	return []*cobra.Command{newUsersCmd(g), newAuthConnectorCmd(g)}
}

func registerConnectors(ctx context.Context, deps ext.Deps, issuer, clientID, secret string) error {
	if deps.Kube.Dynamic == nil {
		return nil
	}
	store := &ConnectorStore{Dynamic: deps.Kube.Dynamic, Namespace: deps.SystemNamespace, Issuer: issuer}
	list, err := store.List(ctx)
	if err != nil {
		if errors.Is(err, ErrNotEnabled) {
			return nil // Dex has not created its CRDs yet; nothing to register
		}
		return fmt.Errorf("list connectors: %w", err)
	}
	for _, c := range list {
		if err := deps.Auth.AddOIDC(ctx, ext.OIDCProvider{
			ID: c.ID, Label: c.Name, Kind: c.Type, ConnectorID: c.ID, Issuer: issuer,
			ClientID: clientID, ClientSecret: secret,
		}); err != nil {
			return fmt.Errorf("connector %s: %w", c.ID, err)
		}
	}
	return nil
}

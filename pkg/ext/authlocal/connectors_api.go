package authlocal

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/install"
)

// The login methods of the workspace (RFC-0033 phase 3): the Workspace
// page lists, adds and removes Dex connectors through these routes, and
// the sign-in page gains or loses the button at once — no server restart.

type connectorHandlers struct {
	deps   ext.Deps
	issuer string
}

func (h *connectorHandlers) store() *ConnectorStore {
	return &ConnectorStore{Dynamic: h.deps.Kube.Dynamic, Namespace: h.deps.SystemNamespace, Issuer: h.issuer}
}

// LoginMethods is GET /api/auth/connectors: the connectors plus the local
// password method, so the page shows every way in.
type LoginMethods struct {
	Password   bool        `json:"password"`
	Connectors []Connector `json:"connectors"`
	Kinds      []string    `json:"kinds"`
	// Callback is the redirect URI to register at the provider.
	Callback string `json:"callback"`
}

func (h *connectorHandlers) list(c *gin.Context) {
	out := LoginMethods{Password: true, Connectors: []Connector{}, Kinds: ConnectorKinds, Callback: strings.TrimRight(h.issuer, "/") + "/callback"}
	if h.deps.Kube != nil && h.deps.Kube.Dynamic != nil {
		list, err := h.store().List(c.Request.Context())
		if err != nil && !errors.Is(err, ErrNotEnabled) {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		if list != nil {
			out.Connectors = list
		}
	}
	c.JSON(http.StatusOK, out)
}

// connectorRequest is POST /api/auth/connectors.
type connectorRequest struct {
	Type         string `json:"type" binding:"required"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	ClientID     string `json:"clientId" binding:"required"`
	ClientSecret string `json:"clientSecret" binding:"required"`
	Org          string `json:"org"`
	HostedDomain string `json:"hostedDomain"`
	Tenant       string `json:"tenant"`
	Issuer       string `json:"issuer"`
}

func (h *connectorHandlers) add(c *gin.Context) {
	var req connectorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	spec := ConnectorSpec{Type: req.Type, ID: req.ID, Name: req.Name, ClientID: strings.TrimSpace(req.ClientID), ClientSecret: strings.TrimSpace(req.ClientSecret),
		Org: strings.TrimSpace(req.Org), HostedDomain: strings.ToLower(strings.TrimSpace(req.HostedDomain)), Tenant: strings.TrimSpace(req.Tenant), Issuer: strings.TrimSpace(req.Issuer)}
	if err := spec.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.deps.Kube == nil || h.deps.Kube.Dynamic == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "no cluster"})
		return
	}
	existed, err := h.store().Add(c.Request.Context(), spec)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	// The button appears now: register the provider with the same Dex
	// client the password method uses.
	if h.deps.Auth != nil {
		sec, err := h.deps.Kube.Kube.CoreV1().Secrets(h.deps.SystemNamespace).Get(c.Request.Context(), install.OIDCClientSecretName, metav1.GetOptions{})
		if err == nil {
			clientID, secret := strings.TrimSpace(string(sec.Data["client-id"])), strings.TrimSpace(string(sec.Data["client-secret"]))
			if err := h.deps.Auth.AddOIDC(c.Request.Context(), ext.OIDCProvider{
				ID: spec.ID, Label: spec.Name, Kind: spec.Type, ConnectorID: spec.ID, Issuer: h.issuer, ClientID: clientID, ClientSecret: secret,
			}); err != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": "connector saved, but the sign-in page could not register it: " + err.Error()})
				return
			}
		}
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	c.JSON(status, Connector{ID: spec.ID, Type: spec.Type, Name: spec.Name})
}

func (h *connectorHandlers) remove(c *gin.Context) {
	id := c.Param("id")
	if h.deps.Kube == nil || h.deps.Kube.Dynamic == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "no cluster"})
		return
	}
	if err := h.store().Remove(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if h.deps.Auth != nil {
		h.deps.Auth.RemoveOIDC(id)
	}
	c.Status(http.StatusNoContent)
}

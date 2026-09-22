package api

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/authz"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/install"
)

// The server is an OpenID Connect relying party (RFC-0007): extensions
// register issuers (Dex for local accounts, Okta, ...), users sign in
// through the authorization code flow with PKCE and get a session cookie.
// The admin token keeps working for automation.

const (
	sessionCookie = "shpyrd_session"
	csrfCookie    = "shpyrd_csrf"
	csrfHeader    = "X-Shpyrd-CSRF"
	loginTTL      = 10 * time.Minute
)

// ProviderInfo is a login option shown by the dashboard.
type ProviderInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// AuthConfig is the auth part of GET /api/config.
type AuthConfig struct {
	// Token says the admin token is accepted.
	Token bool `json:"token"`
	// Providers are the sign-in options, in registration order.
	Providers []ProviderInfo `json:"providers"`
}

type oidcProvider struct {
	ext.OIDCProvider
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

// pendingLogin is an authorization request waiting for its callback.
type pendingLogin struct {
	provider string
	nonce    string
	verifier string
	next     string
	created  time.Time
}

// relyingParty holds providers, in-flight logins and sessions.
type relyingParty struct {
	mu        sync.Mutex
	providers map[string]*oidcProvider
	order     []string
	pending   map[string]pendingLogin
	sessions  *sessionStore
	baseURL   string // external dashboard URL, redirect URIs are built on it
	domain    string
	ingress   string // cluster-internal address reaching the ingress controller
	caPEM     []byte
	log       loggerish
	now       func() time.Time
}

type loggerish interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

func newRelyingParty(sessions *sessionStore, baseURL, domain, ingress string, log loggerish) *relyingParty {
	return &relyingParty{
		providers: map[string]*oidcProvider{}, pending: map[string]pendingLogin{}, sessions: sessions,
		baseURL: strings.TrimRight(baseURL, "/"), domain: domain, ingress: ingress, log: log, now: time.Now,
	}
}

// redirectURI is where issuers send users back.
func (rp *relyingParty) redirectURI() string { return rp.baseURL + "/api/auth/callback" }

// AddOIDC implements ext.AuthRegistry: discover the issuer and keep its
// endpoints. Discovery goes through httpClient so an issuer published on
// the cluster's own domain is reached inside the cluster.
func (rp *relyingParty) AddOIDC(ctx context.Context, p ext.OIDCProvider) error {
	if p.ID == "" || p.Issuer == "" || p.ClientID == "" {
		return errors.New("oidc provider needs id, issuer and client id")
	}
	client := rp.httpClient()
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), p.Issuer)
	if err != nil {
		return fmt.Errorf("discover %s: %w", p.Issuer, err)
	}
	scopes := append([]string{oidc.ScopeOpenID, "email", "profile"}, p.Scopes...)
	op := &oidcProvider{
		OIDCProvider: p,
		oauth: oauth2.Config{
			ClientID: p.ClientID, ClientSecret: p.ClientSecret, Endpoint: provider.Endpoint(),
			RedirectURL: rp.redirectURI(), Scopes: scopes,
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: p.ClientID}),
		client:   client,
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if _, dup := rp.providers[p.ID]; !dup {
		rp.order = append(rp.order, p.ID)
	}
	rp.providers[p.ID] = op
	rp.log.Info("login provider registered", "provider", p.ID, "issuer", p.Issuer)
	return nil
}

// SetClusterCA trusts the cluster's own certificate authority (issuers
// published through the cluster ingress carry its certificates).
func (rp *relyingParty) SetClusterCA(pem []byte) { rp.caPEM = pem }

// httpClient reaches issuers under the cluster domain through the ingress
// controller service: from inside the cluster the public hostname would
// otherwise resolve to the developer's machine (127.0.0.1.nip.io) or hairpin
// through a load balancer.
func (rp *relyingParty) httpClient() *http.Client {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(rp.caPEM) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(rp.caPEM)
		tlsCfg.RootCAs = pool
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err == nil && rp.ingress != "" && rp.domain != "" && strings.HasSuffix(host, "."+rp.domain) {
				return dialer.DialContext(ctx, network, rp.ingress)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second}
}

func (rp *relyingParty) providerList() []ProviderInfo {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	out := make([]ProviderInfo, 0, len(rp.order))
	for _, id := range rp.order {
		out = append(out, ProviderInfo{ID: id, Label: rp.providers[id].Label})
	}
	return out
}

// begin starts the authorization code flow and returns the issuer URL.
func (rp *relyingParty) begin(providerID, next string) (string, error) {
	rp.mu.Lock()
	p, ok := rp.providers[providerID]
	if !ok && providerID == "" && len(rp.order) == 1 {
		p, ok = rp.providers[rp.order[0]], true
		providerID = rp.order[0]
	}
	rp.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown login provider %q", providerID)
	}
	state, err := randomToken(24)
	if err != nil {
		return "", err
	}
	nonce, err := randomToken(24)
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()
	rp.mu.Lock()
	now := rp.now()
	for k, v := range rp.pending {
		if now.Sub(v.created) > loginTTL {
			delete(rp.pending, k)
		}
	}
	rp.pending[state] = pendingLogin{provider: providerID, nonce: nonce, verifier: verifier, next: safeNext(next), created: now}
	rp.mu.Unlock()
	return p.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

// complete exchanges the callback code for an identity.
func (rp *relyingParty) complete(ctx context.Context, state, code string) (ext.Identity, string, error) {
	rp.mu.Lock()
	pl, ok := rp.pending[state]
	delete(rp.pending, state)
	p := rp.providers[pl.provider]
	rp.mu.Unlock()
	if !ok || p == nil || rp.now().Sub(pl.created) > loginTTL {
		return ext.Identity{}, "", errors.New("login expired or unknown; start again")
	}
	tok, err := p.oauth.Exchange(oidc.ClientContext(ctx, p.client), code, oauth2.VerifierOption(pl.verifier))
	if err != nil {
		return ext.Identity{}, "", fmt.Errorf("token exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return ext.Identity{}, "", errors.New("issuer returned no id_token")
	}
	idt, err := p.verifier.Verify(oidc.ClientContext(ctx, p.client), raw)
	if err != nil {
		return ext.Identity{}, "", fmt.Errorf("verify id_token: %w", err)
	}
	if idt.Nonce != pl.nonce {
		return ext.Identity{}, "", errors.New("id_token nonce mismatch")
	}
	var claims struct {
		Email             string   `json:"email"`
		Name              string   `json:"name"`
		PreferredUsername string   `json:"preferred_username"`
		Groups            []string `json:"groups"`
	}
	if err := idt.Claims(&claims); err != nil {
		return ext.Identity{}, "", fmt.Errorf("read claims: %w", err)
	}
	id := ext.Identity{
		Subject: idt.Subject, Email: strings.ToLower(claims.Email), Name: firstNonEmpty(claims.Name, claims.PreferredUsername, claims.Email),
		Groups: claims.Groups, Provider: pl.provider, Admin: true,
	}
	return id, pl.next, nil
}

// safeNext only allows same-origin paths as post-login destinations.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/api/") {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return next
}

// ---- server integration ------------------------------------------------------

// authProviders is the public list of login options.
func (s *Server) authProviders(c *gin.Context) {
	c.JSON(http.StatusOK, s.authConfig())
}

func (s *Server) authConfig() AuthConfig {
	cfg := AuthConfig{Token: s.opts.Token != "", Providers: []ProviderInfo{}}
	if s.rp != nil {
		cfg.Providers = s.rp.providerList()
	}
	return cfg
}

// authLogin redirects the browser to the issuer.
func (s *Server) authLogin(c *gin.Context) {
	if s.rp == nil {
		abort(c, http.StatusNotFound, errors.New("no login provider configured"))
		return
	}
	u, err := s.rp.begin(c.Query("provider"), c.Query("next"))
	if err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	c.Redirect(http.StatusFound, u)
}

// authCallback finishes the flow and sets the session cookies.
func (s *Server) authCallback(c *gin.Context) {
	if s.rp == nil {
		abort(c, http.StatusNotFound, errors.New("no login provider configured"))
		return
	}
	if e := c.Query("error"); e != "" {
		s.loginFailed(c, fmt.Errorf("%s: %s", e, c.Query("error_description")))
		return
	}
	id, next, err := s.rp.complete(c.Request.Context(), c.Query("state"), c.Query("code"))
	if err != nil {
		s.log.Warn("login failed", "error", err, "remote", c.ClientIP())
		s.loginFailed(c, err)
		return
	}
	sess, err := s.rp.sessions.create(c.Request.Context(), id)
	if err != nil {
		abort(c, http.StatusInternalServerError, err)
		return
	}
	s.setSessionCookies(c, sess)
	s.log.Info("user signed in", "email", id.Email, "provider", id.Provider)
	c.Redirect(http.StatusFound, next)
}

// loginFailed sends the browser back to the login page with the reason.
func (s *Server) loginFailed(c *gin.Context, err error) {
	c.Redirect(http.StatusFound, "/?login_error="+url.QueryEscape(err.Error()))
}

func (s *Server) secureCookies() bool {
	return strings.HasPrefix(s.opts.Public.DashboardURL, "https://")
}

func (s *Server) setSessionCookies(c *gin.Context, sess *session) {
	secure := s.secureCookies()
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: sess.ID, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionAbsolute.Seconds())})
	// Readable by the dashboard, echoed in X-Shpyrd-CSRF on mutations.
	http.SetCookie(c.Writer, &http.Cookie{Name: csrfCookie, Value: sess.CSRF, Path: "/", Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionAbsolute.Seconds())})
}

func (s *Server) clearSessionCookies(c *gin.Context) {
	for _, name := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(c.Writer, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: name == sessionCookie, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode})
	}
}

// authLogout ends the session (cookie authenticated, CSRF checked by auth()).
func (s *Server) authLogout(c *gin.Context) {
	if s.rp != nil {
		if sid, err := c.Cookie(sessionCookie); err == nil && sid != "" {
			s.rp.sessions.delete(c.Request.Context(), sid)
		}
	}
	s.clearSessionCookies(c)
	c.Status(http.StatusNoContent)
}

// Me is the caller's identity with the roles the dashboard hides actions by.
type Me struct {
	ext.Identity
	Roles authz.Roles `json:"roles"`
}

// me returns the caller's identity and roles.
func (s *Server) me(c *gin.Context) {
	id, ok := ext.IdentityFrom(c)
	if !ok {
		id = ext.Identity{Subject: "anonymous", Provider: "none", Admin: true}
	}
	roles, err := s.rolesOf(c)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	id.Admin = roles.Platform == shpyrdv1.RolePlatformAdmin
	c.JSON(http.StatusOK, Me{Identity: id, Roles: roles})
}

// securityHeaders hardens every response (RFC-0008). The dashboard is a
// same-origin SPA: scripts and styles come from this server only.
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		if !strings.HasPrefix(c.Request.URL.Path, "/api/") {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		}
		c.Next()
	}
}

// sessionAuth tries the session cookie: on success the identity is set and
// mutations must carry the CSRF header.
func (s *Server) sessionAuth(c *gin.Context) (bool, error) {
	if s.rp == nil {
		return false, nil
	}
	sid, err := c.Cookie(sessionCookie)
	if err != nil || sid == "" {
		return false, nil
	}
	sess, ok := s.rp.sessions.get(sid)
	if !ok {
		s.clearSessionCookies(c)
		return false, nil
	}
	switch c.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if subtle.ConstantTimeCompare([]byte(c.GetHeader(csrfHeader)), []byte(sess.CSRF)) != 1 {
			return false, errors.New("missing or invalid CSRF token")
		}
	}
	ext.SetIdentity(c, sess.Identity)
	return true, nil
}

// clusterCA reads the certificate authority the cluster issuer signs with:
// the bundle trust-manager distributes to every namespace, or the root CA
// Secret itself.
func (s *Server) clusterCA(ctx context.Context) []byte {
	if s.kube == nil || s.kube.Kube == nil {
		return nil
	}
	if cm, err := s.kube.Kube.CoreV1().ConfigMaps(s.kube.Namespace).Get(ctx, install.CABundleName, metav1.GetOptions{}); err == nil {
		if pem := cm.Data[install.CABundleKey]; pem != "" {
			return []byte(pem)
		}
	}
	for _, ns := range []string{"cert-manager", s.kube.Namespace} {
		if sec, err := s.kube.Kube.CoreV1().Secrets(ns).Get(ctx, install.LocalCASecretName, metav1.GetOptions{}); err == nil {
			if pem := sec.Data["ca.crt"]; len(pem) > 0 {
				return pem
			}
			return sec.Data["tls.crt"]
		}
	}
	return nil
}

// rateLimiter is a per-client token bucket for the login endpoints: it
// slows down credential stuffing and callback replay without a store.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	return &rateLimiter{buckets: map[string]*bucket{}, rate: float64(perMinute) / 60, burst: float64(perMinute), now: time.Now}
}

// allow takes a token for key, reporting whether one was available.
func (r *rateLimiter) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.buckets[key]
	if !ok {
		if len(r.buckets) > 10000 {
			r.buckets = map[string]*bucket{} // crude but bounded
		}
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[key] = b
	}
	b.tokens = min(r.burst, b.tokens+now.Sub(b.last).Seconds()*r.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (r *rateLimiter) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !r.allow(c.ClientIP()) {
			c.Header("Retry-After", "60")
			abort(c, http.StatusTooManyRequests, errors.New("too many sign-in attempts; try again in a minute"))
			return
		}
		c.Next()
	}
}

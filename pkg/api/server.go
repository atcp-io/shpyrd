// Package api implements the shpyrd HTTP API and serves the embedded UI.
package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"helm.sh/helm/v3/pkg/action"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"shpyrd/pkg/authz"
	"shpyrd/pkg/edge"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
	"shpyrd/pkg/store"
)

// Options configures the server.
type Options struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// UI is the built single page application. Nil disables UI serving.
	UI fs.FS
	// Sources stores uploaded application archives. Nil disables deploys
	// from local checkouts.
	Sources *SourceStore
	// Token protects the API. Empty disables authentication (development).
	Token string
	// TokenDisabled refuses the admin token: sign-in through accounts only
	// (`shpyrd cluster token --disable`). Authentication stays required.
	TokenDisabled bool
	// Apps is the client used for App resources; defaults to an uncached
	// controller-runtime client built from the kube client. Tests inject a
	// fake.
	Apps client.Client
	// Prometheus serves the metrics endpoints. Nil disables them.
	Prometheus *PromClient
	// Public is returned by GET /api/config for the dashboard.
	Public PublicConfig
	// Extensions are the enabled extensions (RFC-0002); their routes are
	// mounted and their login providers registered.
	Extensions []ext.Extension
	// Vars looks up install variables (SHPYRD_*) for extensions; defaults
	// to the environment.
	Vars func(name string) string
	// IngressService is the cluster-internal address of the ingress
	// controller, used to reach issuers published on the cluster domain.
	IngressService string
	// RegistryGC is the in-cluster registry's garbage collector (RFC-0059);
	// nil when this replica does not run the controller.
	RegistryGC RegistryGC
	// Store is the control-plane database (RFC-0033): teams, grants, the
	// people seen at sign-in, the workspace. Defaults to an in-memory store
	// (tests, development without a database).
	Store store.Store
	// MembershipChanged is called after every team or grant write so the
	// RBAC mirror runs at once; nil when this replica does not run it.
	MembershipChanged func()
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// PublicConfig is what the dashboard needs before logging in.
type PublicConfig struct {
	Version      string `json:"version"`
	Domain       string `json:"domain"`
	HTTPSPort    string `json:"httpsPort"`
	GrafanaURL   string `json:"grafanaUrl"`
	DashboardURL string `json:"dashboardUrl,omitempty"`
	AuthRequired bool   `json:"authRequired"`
	Metrics      bool   `json:"metrics"`
	// Auth lists the sign-in options (RFC-0007).
	Auth AuthConfig `json:"auth"`
	// Extensions enabled on this cluster (RFC-0002).
	Extensions []string `json:"extensions"`
	// Volumes describes the profile's storage rules (RFC-0060).
	Volumes VolumesConfig `json:"volumes"`
}

// VolumesConfig is what the dashboard needs to know about volumes here.
type VolumesConfig struct {
	// MinSize is the provider minimum requests are rounded up to ("" = none).
	MinSize string `json:"minSize,omitempty"`
	// Snapshots is true when the cluster can take volume snapshots.
	Snapshots bool `json:"snapshots"`
}

// Server is the shpyrd API server.
type Server struct {
	opts    Options
	log     *slog.Logger
	engine  *gin.Engine
	kube    *kube.Client
	apps    client.Client
	helm    *action.Configuration
	sources *SourceStore
	prom    *PromClient
	rp      *relyingParty
	authz   *authz.Resolver
	store   store.Store
	// The edge (RFC-0033): signing keys, one-time codes, host index.
	edgeKeys  *edge.Keys
	edgeCodes *edge.Codes
	hostCache hostCache
	// lookupTXT resolves TXT records for domain claims; nil uses the system
	// resolver (tests inject one).
	lookupTXT func(ctx context.Context, name string) ([]string, error)
	// tokenFailures throttles clients presenting wrong admin tokens.
	tokenFailures *rateLimiter
	// passwordFailures throttles wrong passwords per account (RFC-0012).
	passwordFailures *rateLimiter
	// regCache holds the registry catalog summary for a minute.
	regCache registryCache
	// The web terminal (RFC-0026): unredeemed tickets and the live shells.
	execTickets *ticketStore
	shells      *shellRegistry
	// shellMints throttles ticket minting per actor (RFC-0026).
	shellMints *rateLimiter
	// The exec bridge (RFC-0026). The three function fields are the seam tests
	// replace: everything but the pod stream itself is then testable, down to
	// the candidate walk execRun sits under.
	execStream execStreamFunc
	probeShell probeShellFunc
	execRun    execRunFunc
	shellIdle  time.Duration
	shellPing  time.Duration
	shellProbe time.Duration
}

// New wires the routes.
func New(k *kube.Client, opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = ":8080"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if os.Getenv("SHPYRD_DEBUG") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	helmCfg := new(action.Configuration)
	if err := helmCfg.Init(k.RESTClientGetterFor(""), "", os.Getenv("HELM_DRIVER"), func(format string, v ...interface{}) {
		opts.Logger.Debug(fmt.Sprintf(format, v...), "component", "helm")
	}); err != nil {
		return nil, fmt.Errorf("helm: %w", err)
	}
	if opts.Apps == nil {
		apps, err := k.ControllerClient()
		if err != nil {
			return nil, err
		}
		opts.Apps = apps
	}
	return newServer(k, opts, helmCfg)
}

// newServer wires everything except Helm initialisation (tests pass nil).
func newServer(k *kube.Client, opts Options, helmCfg *action.Configuration) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Vars == nil {
		opts.Vars = os.Getenv
	}
	opts.Public.AuthRequired = opts.Token != "" || opts.TokenDisabled
	opts.Public.Metrics = opts.Prometheus != nil
	opts.Public.Extensions = ext.Names(opts.Extensions)
	if opts.Public.Extensions == nil {
		opts.Public.Extensions = []string{}
	}
	if opts.Token == "" && !opts.TokenDisabled {
		opts.Logger.Warn("API authentication disabled: no admin token configured")
	}
	if opts.TokenDisabled {
		opts.Logger.Info("admin token disabled: sign-in through accounts only")
	}
	if opts.Store == nil {
		opts.Store = store.NewMemory()
	}
	s := &Server{opts: opts, log: opts.Logger, kube: k, apps: opts.Apps, helm: helmCfg, sources: opts.Sources, prom: opts.Prometheus, store: opts.Store}
	s.authz = &authz.Resolver{Store: opts.Store}
	s.tokenFailures = newRateLimiter(20)
	s.passwordFailures = newRateLimiter(10)
	s.execTickets = newTicketStore(execTicketTTL)
	s.shells = newShellRegistry()
	s.execStream = s.streamExec
	s.probeShell = s.resolveShell
	s.execRun = s.runKexec
	s.shellIdle = shellIdleTimeout
	s.shellPing = shellPingInterval
	s.shellProbe = shellProbeTimeout
	s.shellMints = newRateLimiter(shellMintsPerMinute)
	s.engine = gin.New()
	s.engine.Use(gin.Recovery(), s.requestLogger(), securityHeaders())
	_ = s.engine.SetTrustedProxies(nil)

	// Sessions and login providers (RFC-0007). Sessions are mirrored into a
	// Secret when a cluster is available.
	var kubeIface kubernetes.Interface
	systemNS := install.DefaultSystemNamespace
	if k != nil {
		kubeIface = k.Kube
		if k.Namespace != "" {
			systemNS = k.Namespace
		}
	}
	sessions := newSessionStore(opts.Store, kubeIface, systemNS, opts.Logger)
	sessions.load(context.Background())
	// The edge's signing key lives in the cluster; tests and clusterless
	// runs get a fresh one.
	if kubeIface != nil {
		keys, err := edge.LoadOrCreateKeys(context.Background(), kubeIface, systemNS)
		if err != nil {
			return nil, fmt.Errorf("edge keys: %w", err)
		}
		s.edgeKeys = keys
	} else {
		keys, err := edge.GenerateKeys()
		if err != nil {
			return nil, err
		}
		s.edgeKeys = keys
	}
	s.edgeCodes = edge.NewCodes(opts.Store)
	s.rp = newRelyingParty(sessions, opts.Public.DashboardURL, opts.Public.Domain, opts.IngressService, opts.Logger)
	s.rp.SetClusterCA(s.clusterCA(context.Background()))

	if err := s.routes(); err != nil {
		return nil, err
	}
	return s, nil
}

// deps is what extensions get from the server.
func (s *Server) deps() ext.Deps {
	ns := install.DefaultSystemNamespace
	if s.kube != nil && s.kube.Namespace != "" {
		ns = s.kube.Namespace
	}
	return ext.Deps{Kube: s.kube, Client: s.apps, SystemNamespace: ns, Vars: s.opts.Vars, Auth: s.rp}
}

// routeGroups implements ext.Router.
type routeGroups struct{ pub, api, admin gin.IRouter }

func (r routeGroups) Public() gin.IRouter    { return r.pub }
func (r routeGroups) Protected() gin.IRouter { return r.api }
func (r routeGroups) Admin() gin.IRouter     { return r.admin }

// Handler exposes the router, e.g. for tests.
func (s *Server) Handler() http.Handler { return s.engine }

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.engine,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", s.opts.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s.log.Info("shutting down")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-errCh
}

func (s *Server) routes() error {
	// The edge (RFC-0033): what ingress-nginx and app hosts call.
	s.engine.GET("/edge/auth", s.edgeAuth)
	s.engine.GET("/.well-known/jwks.json", s.jwks)
	s.engine.GET(edgePathPrefix+"signin", s.edgeSignin)
	s.engine.GET(edgePathPrefix+"start", s.edgeStart)
	s.engine.GET(edgePathPrefix+"callback", s.edgeCallback)
	s.engine.GET(edgePathPrefix+"logout", s.edgeLogout)

	pub := s.engine.Group("/api")
	pub.GET("/healthz", s.healthz)
	pub.GET("/config", s.config)
	// Archives are content addressed (SHA-256) and fetched by build
	// instances, which cannot present the admin token.
	pub.GET("/sources/:name", s.serveSource)
	// The web terminal (RFC-0026) carries no header and may carry no cookie: a
	// browser cannot set headers on a WebSocket, and `shpyrd cluster dashboard`
	// signs in with a token in localStorage. So it skips s.auth() and the
	// one-time ticket is the whole credential; appShell takes the identity from
	// it and re-resolves the role itself.
	pub.GET("/projects/:slug/shell", s.appShell)
	// Sign-in (RFC-0007): the issuer redirects back to /api/auth/callback.
	pub.GET("/auth/providers", s.authProviders)
	login := newRateLimiter(30).middleware()
	pub.GET("/auth/login", login, s.authLogin)
	pub.GET("/auth/callback", login, s.authCallback)
	pub.GET("/auth/ticket", login, s.authTicket)
	pub.POST("/auth/password", login, s.authPassword) // RFC-0012
	pub.POST("/auth/token", login, s.authToken)       // the admin token as a session (RFC-0033)

	// Every protected route names the action it performs (RFC-0008); the
	// caller's roles decide.
	api := s.engine.Group("/api", s.auth())
	api.GET("/me", s.me)
	api.POST("/auth/logout", s.authLogout)
	api.GET("/namespaces", s.require(authz.ClusterView), s.listNamespaces)
	api.GET("/helm/releases", s.require(authz.ClusterView), s.listHelmReleases)
	api.GET("/cluster", s.require(authz.ClusterView), s.clusterSummary)
	api.GET("/cluster/metrics", s.require(authz.ClusterView), s.clusterMetrics)
	api.GET("/cluster/registry", s.require(authz.ClusterAdmin), s.registryInfo) // RFC-0059
	api.POST("/cluster/registry/gc", s.require(authz.ClusterAdmin), s.registryGC)
	api.GET("/cluster/backups", s.require(authz.ClusterAdmin), s.listBackups) // RFC-0037
	api.POST("/cluster/backups", s.require(authz.ClusterAdmin), s.runBackup)
	api.GET("/sizes", s.getSizes) // any signed-in user: the size selector needs it
	api.PUT("/sizes", s.require(authz.ClusterAdmin), s.putSizes)
	api.GET("/globals", s.require(authz.ClusterAdmin), s.getGlobals) // RFC-0016
	api.PUT("/globals", s.require(authz.ClusterAdmin), s.putGlobals)
	// Cluster log drains: every project's lines (RFC-0023).
	api.GET("/drains", s.require(authz.ClusterAdmin), s.listClusterDrains)
	api.POST("/drains", s.require(authz.ClusterAdmin), s.createClusterDrain)
	api.DELETE("/drains/:name", s.require(authz.ClusterAdmin), s.deleteClusterDrain)
	api.POST("/sources", s.uploadSource) // deploys check the project right when the App is updated
	// The workspace (RFC-0033): its name and the people it has seen.
	api.GET("/workspace", s.getWorkspace) // any signed-in user: the dashboard shows the name
	api.PATCH("/workspace", s.require(authz.ClusterAdmin), s.updateWorkspace)
	api.GET("/workspace/people", s.require(authz.ClusterAdmin), s.listPeople)
	api.DELETE("/workspace/people/:email", s.require(authz.ClusterAdmin), s.forgetPerson)
	api.PATCH("/workspace/people/:email", s.require(authz.ClusterAdmin), s.setPersonStatus)
	api.GET("/workspace/domain-claims", s.require(authz.ClusterAdmin), s.listDomainClaims)
	api.POST("/workspace/domain-claims", s.require(authz.ClusterAdmin), s.putDomainClaim)
	api.POST("/workspace/domain-claims/:domain/verify", s.require(authz.ClusterAdmin), s.verifyDomainClaim)
	api.DELETE("/workspace/domain-claims/:domain", s.require(authz.ClusterAdmin), s.deleteDomainClaim)
	api.GET("/workspace/grants", s.require(authz.ClusterAdmin), s.listAllMembers)
	api.GET("/workspace/export", s.require(authz.ClusterAdmin), s.exportWorkspace) // RFC-0037
	api.POST("/workspace/import", s.require(authz.ClusterAdmin), s.importWorkspace)
	api.GET("/teams", s.require(authz.ClusterAdmin), s.listTeams)
	api.POST("/teams", s.require(authz.ClusterAdmin), s.putTeam)
	api.PUT("/teams/:name", s.require(authz.ClusterAdmin), s.putTeam)
	api.DELETE("/teams/:name", s.require(authz.ClusterAdmin), s.deleteTeam)

	// Projects (RFC-0011): every path names the project by its slug; the
	// server derives the namespace (app-<slug>).
	api.GET("/projects", s.listApps) // filtered to visible projects
	api.POST("/projects", s.require(authz.ClusterCreate), s.createApp)
	api.GET("/projects/:slug", s.require(authz.ProjectView), s.getApp)
	api.PATCH("/projects/:slug", s.require(authz.ProjectConfig), s.updateApp)
	api.DELETE("/projects/:slug", s.require(authz.ProjectDestroy), s.deleteApp)
	api.POST("/projects/:slug/deploy", s.require(authz.ProjectDeploy), s.deployApp)
	api.GET("/projects/:slug/logs", s.require(authz.ProjectView), s.appLogs)
	api.GET("/projects/:slug/builds", s.require(authz.ProjectView), s.listBuilds)
	api.GET("/projects/:slug/builds/:build/logs", s.require(authz.ProjectView), s.buildLogs)
	api.GET("/projects/:slug/secrets", s.require(authz.ProjectView), s.appSecretKeys)
	api.PUT("/projects/:slug/secrets", s.require(authz.ProjectConfig), s.updateAppSecrets)
	api.GET("/projects/:slug/metrics", s.require(authz.ProjectView), s.appMetrics)
	api.POST("/projects/:slug/scale", s.require(authz.ProjectScale), s.scaleApp)
	api.POST("/projects/:slug/resize", s.require(authz.ProjectScale), s.resizeApp)
	api.POST("/projects/:slug/processes", s.require(authz.ProjectScale), s.applyProcesses)
	api.POST("/projects/:slug/rollback", s.require(authz.ProjectDeploy), s.rollbackApp)
	api.POST("/projects/:slug/redeploy", s.require(authz.ProjectDeploy), s.redeployApp)
	api.PUT("/projects/:slug/exposure", s.require(authz.ProjectDeploy), s.setExposure)
	api.PUT("/projects/:slug/access", s.require(authz.ProjectMembers), s.setAccess) // RFC-0033: who may open the app
	api.GET("/projects/:slug/allow", s.require(authz.ProjectView), s.listAllow)     // RFC-0033: who may reach from inside
	api.PUT("/projects/:slug/allow", s.require(authz.ProjectMembers), s.setAllow)
	api.POST("/projects/:slug/preview", s.require(authz.ProjectDeploy), s.previewApp) // "Open as"
	api.GET("/launcher", s.launcher)                                                  // the apps the caller may open
	api.GET("/projects/:slug/domains", s.require(authz.ProjectView), s.listDomains)   // RFC-0034
	api.POST("/projects/:slug/domains", s.require(authz.ProjectConfig), s.addDomain)
	api.DELETE("/projects/:slug/domains/:host", s.require(authz.ProjectConfig), s.removeDomain)
	api.GET("/projects/:slug/audit", s.require(authz.ProjectView), s.appAudit)
	api.GET("/projects/:slug/instances", s.require(authz.ProjectExec), s.listInstances) // RFC-0026
	// RFC-0026; the socket itself is on pub. The throttle comes first because it
	// is the cheaper check and because minting is what the socket costs: see
	// throttleShellMints.
	api.POST("/projects/:slug/shell/ticket", s.throttleShellMints(), s.require(authz.ProjectExec), s.mintShellTicket)
	// Project resources (RFC-0003/0006) live in the project namespace.
	api.GET("/projects/:slug/resources", s.require(authz.ProjectView), s.listProjectResources)
	api.POST("/projects/:slug/resources", s.require(authz.ProjectResource), s.createResource)
	api.DELETE("/projects/:slug/resources/:kind/:name", s.require(authz.ProjectResource), s.deleteResource)
	api.POST("/projects/:slug/bindings", s.require(authz.ProjectResource), s.attachResource)
	api.DELETE("/projects/:slug/bindings/:kind/:name", s.require(authz.ProjectResource), s.detachResource)
	api.GET("/projects/:slug/volumes", s.require(authz.ProjectView), s.listVolumes)
	api.POST("/projects/:slug/volumes", s.require(authz.ProjectResource), s.createVolume)
	api.PUT("/projects/:slug/volumes/:name", s.require(authz.ProjectResource), s.resizeVolume)
	api.DELETE("/projects/:slug/volumes/:name", s.require(authz.ProjectResource), s.deleteVolume)
	api.GET("/projects/:slug/volumes/:name/snapshots", s.require(authz.ProjectView), s.listSnapshots)
	api.POST("/projects/:slug/volumes/:name/snapshots", s.require(authz.ProjectResource), s.createSnapshot)
	api.DELETE("/projects/:slug/volumes/:name/snapshots/:snap", s.require(authz.ProjectResource), s.deleteSnapshot)
	api.POST("/projects/:slug/volumes/:name/restore", s.require(authz.ProjectResource), s.restoreVolume)
	// Project log drains (RFC-0023).
	api.GET("/projects/:slug/drains", s.require(authz.ProjectView), s.listProjectDrains)
	api.POST("/projects/:slug/drains", s.require(authz.ProjectResource), s.createProjectDrain)
	api.DELETE("/projects/:slug/drains/:name", s.require(authz.ProjectResource), s.deleteProjectDrain)
	api.GET("/projects/:slug/members", s.require(authz.ProjectMembers), s.listMembers)
	api.POST("/projects/:slug/members", s.require(authz.ProjectMembers), s.addMember)
	api.DELETE("/projects/:slug/members/:name", s.require(authz.ProjectMembers), s.removeMember)

	// Extensions mount their routes and register login providers.
	deps := s.deps()
	for _, x := range s.opts.Extensions {
		if err := x.Routes(routeGroups{pub: pub, api: api, admin: api.Group("", s.require(authz.ClusterAdmin))}, deps); err != nil {
			return fmt.Errorf("extension %s: %w", x.Name(), err)
		}
	}

	if s.opts.UI != nil {
		s.engine.NoRoute(s.serveUI())
	} else {
		s.engine.NoRoute(func(c *gin.Context) {
			if s.customError(c) {
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		})
	}
	return nil
}

// auth accepts the admin token as a Bearer token or in X-Shpyrd-Token (the
// API server's service proxy strips Authorization when the CLI uploads
// through it).
func (s *Server) auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.opts.Token == "" && !s.opts.TokenDisabled {
			c.Next()
			return
		}
		tok := c.GetHeader("X-Shpyrd-Token")
		if h := c.GetHeader("Authorization"); tok == "" && strings.HasPrefix(strings.ToLower(h), "bearer ") {
			tok = strings.TrimSpace(h[7:])
		}
		if tok != "" {
			if s.opts.TokenDisabled {
				abort(c, http.StatusUnauthorized, errors.New("the admin token is disabled on this cluster; sign in with your account"))
				return
			}
			// Wrong tokens are throttled per client and audited; while a
			// client is throttled even the right token is refused, which
			// blunts brute force.
			ip := c.ClientIP()
			if s.tokenFailures.exhausted(ip) {
				c.Header("Retry-After", "60")
				abort(c, http.StatusTooManyRequests, errors.New("too many failed token attempts; try again in a minute"))
				return
			}
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.opts.Token)) != 1 {
				s.tokenFailures.allow(ip)
				s.log.Warn("invalid admin token", "remote", ip, "path", c.Request.URL.Path)
				s.auditAnonymous(c, "auth.token_failed", c.Request.URL.Path)
				c.Header("WWW-Authenticate", `Bearer realm="shpyrd"`)
				abort(c, http.StatusUnauthorized, errors.New("missing or invalid token"))
				return
			}
			ext.SetIdentity(c, ext.Identity{Subject: "admin-token", Name: "admin token", Provider: "token", Admin: true})
			c.Next()
			return
		}
		// A signed-in user (RFC-0007).
		ok, err := s.sessionAuth(c)
		if err != nil {
			abort(c, http.StatusForbidden, err)
			return
		}
		if !ok {
			c.Header("WWW-Authenticate", `Bearer realm="shpyrd"`)
			abort(c, http.StatusUnauthorized, errors.New("missing or invalid token"))
			return
		}
		c.Next()
	}
}

func (s *Server) config(c *gin.Context) {
	pub := s.opts.Public
	pub.Auth = s.authConfig()
	pub.Volumes = VolumesConfig{MinSize: s.vars(install.VarVolumeMinSize), Snapshots: s.vars(install.VarSnapshotClass) != ""}
	c.JSON(http.StatusOK, pub)
}

// serveUI serves the embedded SPA. Unknown non-API paths fall back to
// index.html so client-side routing works.
func (s *Server) serveUI() gin.HandlerFunc {
	fileServer := http.FileServer(http.FS(s.opts.UI))
	return func(c *gin.Context) {
		if s.customError(c) { // ingress-nginx's error backend for app hosts
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		p := strings.TrimPrefix(c.Request.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(s.opts.UI, p); err != nil {
			if _, err := fs.Stat(s.opts.UI, "index.html"); err != nil {
				c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(placeholderHTML))
				return
			}
			c.Request.URL.Path = "/"
		}
		fileServer.ServeHTTP(c.Writer, c.Request)
	}
}

func (s *Server) requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if strings.HasPrefix(c.Request.URL.Path, "/api/") && c.Request.URL.Path != "/api/healthz" {
			s.log.Info("request",
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"status", c.Writer.Status(),
				"duration", time.Since(start).Round(time.Millisecond).String(),
			)
		}
	}
}

func abort(c *gin.Context, status int, err error) {
	c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
}

const placeholderHTML = `<!doctype html><html><head><title>shpyrd</title></head>
<body style="font-family:system-ui;background:#111;color:#eee;padding:2rem">
<h1>shpyrd</h1><p>The UI has not been built into this binary. Run <code>make ui</code> and rebuild,
or use the API at <code>/api/healthz</code>.</p></body></html>`

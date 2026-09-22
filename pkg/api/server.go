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

	"shpyrd/pkg/ext"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
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
	opts.Public.AuthRequired = opts.Token != ""
	opts.Public.Metrics = opts.Prometheus != nil
	opts.Public.Extensions = ext.Names(opts.Extensions)
	if opts.Public.Extensions == nil {
		opts.Public.Extensions = []string{}
	}
	if opts.Token == "" {
		opts.Logger.Warn("API authentication disabled: no admin token configured")
	}
	s := &Server{opts: opts, log: opts.Logger, kube: k, apps: opts.Apps, helm: helmCfg, sources: opts.Sources, prom: opts.Prometheus}
	s.engine = gin.New()
	s.engine.Use(gin.Recovery(), s.requestLogger())
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
	sessions := newSessionStore(kubeIface, systemNS, opts.Logger)
	sessions.load(context.Background())
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
type routeGroups struct{ pub, api gin.IRouter }

func (r routeGroups) Public() gin.IRouter    { return r.pub }
func (r routeGroups) Protected() gin.IRouter { return r.api }

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
	pub := s.engine.Group("/api")
	pub.GET("/healthz", s.healthz)
	pub.GET("/config", s.config)
	// Archives are content addressed (SHA-256) and fetched by kpack build
	// pods, which cannot present the admin token.
	pub.GET("/sources/:name", s.serveSource)
	// Sign-in (RFC-0007): the issuer redirects back to /api/auth/callback.
	pub.GET("/auth/providers", s.authProviders)
	pub.GET("/auth/login", s.authLogin)
	pub.GET("/auth/callback", s.authCallback)

	api := s.engine.Group("/api", s.auth())
	api.GET("/me", s.me)
	api.POST("/auth/logout", s.authLogout)
	api.GET("/namespaces", s.listNamespaces)
	api.GET("/helm/releases", s.listHelmReleases)
	api.GET("/cluster", s.clusterSummary)
	api.GET("/cluster/metrics", s.clusterMetrics)
	api.GET("/sizes", s.getSizes)
	api.PUT("/sizes", s.putSizes)
	api.POST("/sources", s.uploadSource)

	api.GET("/apps", s.listApps)
	api.POST("/apps", s.createApp)
	api.GET("/apps/:ns/:name", s.getApp)
	api.DELETE("/apps/:ns/:name", s.deleteApp)
	api.POST("/apps/:ns/:name/deploy", s.deployApp)
	api.GET("/apps/:ns/:name/logs", s.appLogs)
	api.GET("/apps/:ns/:name/builds", s.listBuilds)
	api.GET("/apps/:ns/:name/builds/:build/logs", s.buildLogs)
	api.GET("/apps/:ns/:name/secrets", s.appSecretKeys)
	api.PUT("/apps/:ns/:name/secrets", s.updateAppSecrets)
	api.GET("/apps/:ns/:name/metrics", s.appMetrics)
	api.POST("/apps/:ns/:name/scale", s.scaleApp)
	api.POST("/apps/:ns/:name/resize", s.resizeApp)
	api.POST("/apps/:ns/:name/processes", s.applyProcesses)
	api.POST("/apps/:ns/:name/rollback", s.rollbackApp)
	// Project resources (RFC-0003/0006): volumes live in the project namespace.
	api.GET("/projects/:ns/resources", s.listProjectResources)
	api.GET("/projects/:ns/volumes", s.listVolumes)
	api.POST("/projects/:ns/volumes", s.createVolume)
	api.PUT("/projects/:ns/volumes/:name", s.resizeVolume)
	api.DELETE("/projects/:ns/volumes/:name", s.deleteVolume)

	// Extensions mount their routes and register login providers.
	deps := s.deps()
	for _, x := range s.opts.Extensions {
		if err := x.Routes(routeGroups{pub: pub, api: api}, deps); err != nil {
			return fmt.Errorf("extension %s: %w", x.Name(), err)
		}
	}

	if s.opts.UI != nil {
		s.engine.NoRoute(s.serveUI())
	} else {
		s.engine.NoRoute(func(c *gin.Context) {
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
		if s.opts.Token == "" {
			c.Next()
			return
		}
		tok := c.GetHeader("X-Shpyrd-Token")
		if h := c.GetHeader("Authorization"); tok == "" && strings.HasPrefix(strings.ToLower(h), "bearer ") {
			tok = strings.TrimSpace(h[7:])
		}
		if tok != "" {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.opts.Token)) != 1 {
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
	c.JSON(http.StatusOK, pub)
}

// serveUI serves the embedded SPA. Unknown non-API paths fall back to
// index.html so client-side routing works.
func (s *Server) serveUI() gin.HandlerFunc {
	fileServer := http.FileServer(http.FS(s.opts.UI))
	return func(c *gin.Context) {
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

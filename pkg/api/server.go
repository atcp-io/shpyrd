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
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// PublicConfig is what the dashboard needs before logging in.
type PublicConfig struct {
	Version      string `json:"version"`
	Domain       string `json:"domain"`
	HTTPSPort    string `json:"httpsPort"`
	GrafanaURL   string `json:"grafanaUrl"`
	AuthRequired bool   `json:"authRequired"`
	Metrics      bool   `json:"metrics"`
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
	return newServer(k, opts, helmCfg), nil
}

// newServer wires everything except Helm initialisation (tests pass nil).
func newServer(k *kube.Client, opts Options, helmCfg *action.Configuration) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	opts.Public.AuthRequired = opts.Token != ""
	opts.Public.Metrics = opts.Prometheus != nil
	if opts.Token == "" {
		opts.Logger.Warn("API authentication disabled: no admin token configured")
	}
	s := &Server{opts: opts, log: opts.Logger, kube: k, apps: opts.Apps, helm: helmCfg, sources: opts.Sources, prom: opts.Prometheus}
	s.engine = gin.New()
	s.engine.Use(gin.Recovery(), s.requestLogger())
	_ = s.engine.SetTrustedProxies(nil)
	s.routes()
	return s
}

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

func (s *Server) routes() {
	pub := s.engine.Group("/api")
	pub.GET("/healthz", s.healthz)
	pub.GET("/config", s.config)
	// Archives are content addressed (SHA-256) and fetched by kpack build
	// pods, which cannot present the admin token.
	pub.GET("/sources/:name", s.serveSource)

	api := s.engine.Group("/api", s.auth())
	api.GET("/namespaces", s.listNamespaces)
	api.GET("/helm/releases", s.listHelmReleases)
	api.GET("/cluster", s.clusterSummary)
	api.GET("/cluster/metrics", s.clusterMetrics)
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
	api.POST("/apps/:ns/:name/rollback", s.rollbackApp)

	if s.opts.UI != nil {
		s.engine.NoRoute(s.serveUI())
	} else {
		s.engine.NoRoute(func(c *gin.Context) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		})
	}
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
		if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.opts.Token)) != 1 {
			c.Header("WWW-Authenticate", `Bearer realm="shpyrd"`)
			abort(c, http.StatusUnauthorized, errors.New("missing or invalid token"))
			return
		}
		c.Next()
	}
}

func (s *Server) config(c *gin.Context) {
	c.JSON(http.StatusOK, s.opts.Public)
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

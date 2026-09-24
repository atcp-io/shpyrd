// Command shpyrd-server runs the shpyrd API, the App controller and serves the
// dashboard from inside the cluster (or locally against a kubeconfig).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"k8s.io/apimachinery/pkg/runtime/schema"
	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/internal/controller"

	"shpyrd/pkg/api"
	"shpyrd/pkg/buildtrust"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/ext/all"
	"shpyrd/pkg/kube"
	"shpyrd/pkg/version"
	"shpyrd/ui"
)

func main() {
	// A private FlagSet: controller-runtime registers its own --kubeconfig
	// on flag.CommandLine at init time.
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	var (
		addr        = fs.String("addr", envOr("SHPYRD_ADDR", ":8080"), "listen address")
		metricsAddr = fs.String("metrics-addr", envOr("SHPYRD_METRICS_ADDR", ":8081"), "controller metrics address (0 to disable)")
		kubeconfig  = fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig (defaults to in-cluster, then ~/.kube/config)")
		kubeCtx     = fs.String("context", "", "kubeconfig context")
		dataDir     = fs.String("data-dir", envOr("SHPYRD_DATA_DIR", "/data"), "directory for uploaded source archives")
		internalURL = fs.String("internal-url", os.Getenv("SHPYRD_INTERNAL_URL"), "cluster-internal URL of this server (for kpack blob sources)")
		noControl   = fs.Bool("no-controller", os.Getenv("SHPYRD_NO_CONTROLLER") != "", "serve the API only")
		leaderElect = fs.Bool("leader-elect", os.Getenv("SHPYRD_LEADER_ELECT") != "", "enable leader election for the controller")
		debug       = fs.Bool("debug", os.Getenv("SHPYRD_DEBUG") != "", "verbose logging")
	)
	_ = fs.Parse(os.Args[1:])

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	if err := run(runOptions{
		addr: *addr, metricsAddr: *metricsAddr, kubeconfig: *kubeconfig, kubeCtx: *kubeCtx,
		dataDir: *dataDir, internalURL: *internalURL, controller: !*noControl, leaderElect: *leaderElect,
	}, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type runOptions struct {
	addr, metricsAddr, kubeconfig, kubeCtx string
	dataDir, internalURL                   string
	controller, leaderElect                bool
}

func run(o runOptions, logger *slog.Logger) error {
	k, err := kube.Connect(kube.Options{Kubeconfig: o.kubeconfig, Context: o.kubeCtx})
	if err != nil {
		return fmt.Errorf("connect to cluster: %w", err)
	}
	logger.Info("connected", "host", k.Config.Host, "namespace", k.Namespace)

	internalURL := o.internalURL
	if internalURL == "" {
		internalURL = fmt.Sprintf("http://shpyrd-server.%s.svc", k.Namespace)
	}
	domain := envOr("SHPYRD_DOMAIN", "127.0.0.1.nip.io")
	httpsPort := envOr("SHPYRD_HTTPS_PORT", "443")
	grafana := "https://grafana." + domain
	if httpsPort != "443" {
		grafana += ":" + httpsPort
	}
	var prom *api.PromClient
	if u := envOr("SHPYRD_PROMETHEUS_URL", "http://monitoring-prometheus.monitoring.svc:9090"); u != "" && u != "off" {
		prom = api.NewPromClient(u)
	}
	dashboard := envOr("SHPYRD_DASHBOARD_URL", "https://shpyrd."+domain)
	if os.Getenv("SHPYRD_DASHBOARD_URL") == "" && httpsPort != "443" {
		dashboard += ":" + httpsPort
	}
	// Extensions enabled on this cluster (RFC-0002): recorded by the
	// installer in SHPYRD_EXTENSIONS.
	extensions, unknown := all.Enabled(os.Getenv("SHPYRD_EXTENSIONS"))
	for _, name := range unknown {
		logger.Warn("unknown extension in SHPYRD_EXTENSIONS, ignored", "extension", name)
	}
	if len(extensions) > 0 {
		logger.Info("extensions enabled", "extensions", ext.Names(extensions))
	}
	srv, err := api.New(k, api.Options{
		Addr:           o.addr,
		UI:             ui.Dist(),
		Sources:        &api.SourceStore{Dir: o.dataDir, BaseURL: internalURL},
		Token:          strings.TrimSpace(os.Getenv("SHPYRD_ADMIN_TOKEN")),
		TokenDisabled:  strings.TrimSpace(os.Getenv("SHPYRD_ADMIN_TOKEN_DISABLED")) == "true",
		Prometheus:     prom,
		Extensions:     extensions,
		IngressService: envOr("SHPYRD_INGRESS_SERVICE", "ingress-nginx-controller.ingress-nginx.svc:443"),
		Public: api.PublicConfig{
			Version:      version.Version,
			Domain:       domain,
			HTTPSPort:    httpsPort,
			GrafanaURL:   grafana,
			DashboardURL: dashboard,
		},
		Logger: logger,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(ctx) })

	// Build pods trust the platform CA through this webhook (RFC-0059).
	if addr := os.Getenv("SHPYRD_WEBHOOK_ADDR"); addr != "" {
		hook, err := buildtrust.New(buildtrust.Options{
			Addr:   addr,
			TLSDir: envOr("SHPYRD_WEBHOOK_TLS_DIR", "/etc/shpyrd/webhook-tls"),
			Bundle: envOr("SHPYRD_CA_BUNDLE", "shpyrd-ca-bundle"),
			Logger: logger,
		})
		if err != nil {
			return err
		}
		g.Go(func() error { return hook.Run(ctx) })
	}

	if o.controller {
		mgr, err := newManager(k, o)
		if err != nil {
			return err
		}
		g.Go(func() error {
			logger.Info("starting controller manager")
			return mgr.Start(ctx)
		})
	}
	return g.Wait()
}

func newManager(k *kube.Client, o runOptions) (ctrl.Manager, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := shpyrdv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(k.Config, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress:  "0",
		LeaderElection:          o.leaderElect,
		LeaderElectionID:        "shpyrd-server",
		LeaderElectionNamespace: k.Namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("controller manager: %w", err)
	}
	enabledExts, _ := all.Enabled(os.Getenv("SHPYRD_EXTENSIONS"))
	var bindable []schema.GroupVersionKind
	for _, t := range all.BindableTypes(enabledExts) {
		bindable = append(bindable, schema.GroupVersionKind{Group: t.Group, Version: t.Version, Kind: t.Kind})
	}
	rec := &controller.AppReconciler{
		BindableTypes: bindable,
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Recorder:      mgr.GetEventRecorderFor("shpyrd"),
		Config: controller.Config{
			Domain:           os.Getenv("SHPYRD_DOMAIN"),
			HTTPSPort:        os.Getenv("SHPYRD_HTTPS_PORT"),
			RegistryHost:     os.Getenv("SHPYRD_REGISTRY_HOST"),
			ClusterIssuer:    os.Getenv("SHPYRD_CLUSTER_ISSUER"),
			IngressClass:     os.Getenv("SHPYRD_INGRESS_CLASS"),
			SystemNamespace:  k.Namespace,
			BuildKitImage:    os.Getenv("SHPYRD_BUILDKIT_IMAGE"),
			PodCIDR:          os.Getenv("SHPYRD_POD_CIDR"),
			RegistrySecret:   os.Getenv("SHPYRD_REGISTRY_SECRET"),
			RegistryInsecure: registryInsecure(os.Getenv("SHPYRD_REGISTRY_INSECURE"), os.Getenv("SHPYRD_REGISTRY_HOST")),
			CABundle:         envOr("SHPYRD_CA_BUNDLE", "shpyrd-ca-bundle"),
		},
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("app controller: %w", err)
	}
	drains := &controller.LogDrainReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorderFor("shpyrd"), SystemNamespace: k.Namespace}
	if err := drains.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("log drain controller: %w", err)
	}
	volumes := &controller.VolumeReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorderFor("shpyrd")}
	if err := volumes.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("volume controller: %w", err)
	}
	memberships := &controller.MembershipReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
	if err := memberships.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("membership controller: %w", err)
	}
	deps := ext.Deps{Kube: k, Client: mgr.GetClient(), SystemNamespace: k.Namespace, Vars: os.Getenv}
	for _, x := range enabledExts {
		if err := x.Register(mgr, deps); err != nil {
			return nil, fmt.Errorf("extension %s: %w", x.Name(), err)
		}
	}
	return mgr, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// registryInsecure follows SHPYRD_REGISTRY_INSECURE; every registry,
// including the in-cluster one (RFC-0059), speaks TLS unless it says so.
func registryInsecure(flag, host string) bool {
	_ = host
	return flag == "true"
}

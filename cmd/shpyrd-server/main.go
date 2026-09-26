// Command shpyrd-server runs the shpyrd API, the App controller and serves the
// dashboard from inside the cluster (or locally against a kubeconfig).
package main

import (
	"context"
	"net/url"
	"time"

	"flag"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/internal/controller"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/shpyrd-io/shpyrd/pkg/api"
	"github.com/shpyrd-io/shpyrd/pkg/buildtrust"
	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/ext/all"
	"github.com/shpyrd-io/shpyrd/pkg/install"
	"github.com/shpyrd-io/shpyrd/pkg/kube"
	"github.com/shpyrd-io/shpyrd/pkg/store"
	"github.com/shpyrd-io/shpyrd/pkg/tenancy"
	"github.com/shpyrd-io/shpyrd/pkg/version"
	"github.com/shpyrd-io/shpyrd/ui"
)

func main() {
	// `shpyrd-server backup`: one platform backup, then exit (RFC-0037).
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		if err := runBackup(logger); err != nil {
			logger.Error("backup failed", "err", err.Error())
			os.Exit(1)
		}
		return
	}
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
	// The in-cluster registry's garbage collector (RFC-0059): the API starts
	// it on demand, the controller manager runs its schedule on the leader.
	var registryGC *controller.RegistryGC
	if os.Getenv("SHPYRD_REGISTRY_IP") != "" && o.controller {
		registryGC = &controller.RegistryGC{
			Namespace: k.Namespace,
			Schedule:  envOr("SHPYRD_REGISTRY_GC", controller.DefaultRegistryGCSchedule),
			Image:     envOr("SHPYRD_REGISTRY_IMAGE", "docker.io/library/registry:3"),
			Logger:    logger,
		}
	}

	// The control-plane store (RFC-0033): teams, grants, people, the
	// workspace. Postgres from SHPYRD_DATABASE_URL (the control-plane-db
	// component or a managed database); memory only for development.
	st, err := openStore(logger, k, domain)
	if err != nil {
		return err
	}
	defer st.Close()

	// The RBAC mirror runs in the controller manager; the API pokes it
	// after every team or grant write.
	memberships := &controller.MembershipReconciler{Store: st}

	// The open-source platform resolves every host to its one workspace.
	// SHPYRD_DEV_TENANCY=address switches to host-based resolution for
	// developing the seam: nothing in this binary creates a second
	// workspace, so it changes nothing on an install (RFC-0033 phase 6).
	var resolver tenancy.Resolver
	if os.Getenv("SHPYRD_DEV_TENANCY") == "address" {
		dashboardHost := dashboard
		if u, err := url.Parse(dashboard); err == nil && u.Host != "" {
			dashboardHost = u.Host
		}
		resolver = &tenancy.ByAddress{Store: st, Domain: domain, DashboardHost: dashboardHost}
		logger.Info("tenancy: workspaces resolved from the request host (development switch)")
	}

	srv, err := api.New(k, api.Options{
		Addr:    o.addr,
		Store:   st,
		Tenancy: resolver,
		MembershipChanged: func() {
			if o.controller {
				memberships.Notify()
			}
		},
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
		Logger:     logger,
		RegistryGC: gcOrNil(registryGC),
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
		mgr, err := newManager(k, o, memberships)
		if err != nil {
			return err
		}
		if registryGC != nil {
			registryGC.Client = mgr.GetClient()
			registryGC.Reader = mgr.GetAPIReader()
			if err := mgr.Add(registryGC); err != nil {
				return fmt.Errorf("registry garbage collector: %w", err)
			}
		}
		if os.Getenv(install.VarRegistryIP) != "" {
			// The in-cluster registry restarts when its certificate is renewed (RFC-0059).
			rotation := &controller.RegistryRotation{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Namespace: k.Namespace, Secret: "registry-tls", Deploy: "registry"}
			if err := mgr.Add(rotation); err != nil {
				return fmt.Errorf("registry rotation: %w", err)
			}
		}
		g.Go(func() error {
			logger.Info("starting controller manager")
			return mgr.Start(ctx)
		})
	}
	return g.Wait()
}

// openStore connects to the control-plane database, migrates it, and
// imports the Team and ProjectMember objects of installs that predate it.
func openStore(logger *slog.Logger, k *kube.Client, domain string) (store.Store, error) {
	url := strings.TrimSpace(os.Getenv("SHPYRD_DATABASE_URL"))
	if url == "" {
		if os.Getenv("SHPYRD_DEV_MEMORY_STORE") == "" {
			return nil, fmt.Errorf("SHPYRD_DATABASE_URL is not set: the control-plane database is required (the control-plane-db component provides it; SHPYRD_DEV_MEMORY_STORE=1 runs without one for development, losing teams on restart)")
		}
		logger.Warn("running with an in-memory control-plane store: teams and grants are lost on restart")
		return store.NewMemory(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var st *store.Postgres
	var err error
	for attempt := 1; ; attempt++ {
		st, err = store.Open(ctx, url)
		if err == nil {
			break
		}
		if attempt >= 12 {
			return nil, err
		}
		logger.Info("waiting for the control-plane database", "attempt", attempt, "err", err.Error())
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(5 * time.Second):
		}
	}
	if err := st.Migrate(ctx, domain); err != nil {
		st.Close()
		return nil, fmt.Errorf("control-plane database: %w", err)
	}
	if teams, grants, err := store.ImportCRDs(ctx, k.Dynamic, st, store.DefaultWorkspace); err != nil {
		logger.Warn("importing teams and members from Kubernetes objects failed; they stay where they are", "err", err.Error())
	} else if teams+grants > 0 {
		logger.Info("imported teams and members into the control-plane database", "teams", teams, "grants", grants)
	}
	return st, nil
}

func newManager(k *kube.Client, o runOptions, memberships *controller.MembershipReconciler) (ctrl.Manager, error) {
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
	workspaceCache := &tenancy.Addresses{Store: memberships.Store}
	rec := &controller.AppReconciler{
		BindableTypes: bindable,
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Recorder:      mgr.GetEventRecorderFor("shpyrd"),
		Config: controller.Config{
			Domain:               os.Getenv("SHPYRD_DOMAIN"),
			WorkspaceDomain:      workspaceCache.Address,
			WorkspaceLimits:      workspaceCache.Limits,
			HTTPSPort:            os.Getenv("SHPYRD_HTTPS_PORT"),
			RegistryHost:         os.Getenv("SHPYRD_REGISTRY_HOST"),
			ClusterIssuer:        os.Getenv("SHPYRD_CLUSTER_ISSUER"),
			IngressClass:         os.Getenv("SHPYRD_INGRESS_CLASS"),
			SystemNamespace:      k.Namespace,
			BuildKitImage:        os.Getenv("SHPYRD_BUILDKIT_IMAGE"),
			PodCIDR:              os.Getenv("SHPYRD_POD_CIDR"),
			RegistrySecret:       os.Getenv("SHPYRD_REGISTRY_SECRET"),
			RegistryInsecure:     registryInsecure(os.Getenv("SHPYRD_REGISTRY_INSECURE"), os.Getenv("SHPYRD_REGISTRY_HOST")),
			CABundle:             envOr("SHPYRD_CA_BUNDLE", "shpyrd-ca-bundle"),
			RegistryDeletes:      os.Getenv("SHPYRD_REGISTRY_IP") != "",
			WildcardTLS:          os.Getenv("SHPYRD_WILDCARD_TLS") == "true",
			IngressClassExternal: envOr("SHPYRD_INGRESS_CLASS_EXTERNAL", "nginx"),
			IngressClassInternal: envOr("SHPYRD_INGRESS_CLASS_INTERNAL", "nginx-internal"),
			InternalLBAddress:    internalLBAddress(k),
			ExternalLBAddress:    externalLBAddress(k),
		},
		// The external address may not exist yet at start (first install):
		// look it up when a custom domain needs it.
		LookupLB: func(context.Context) string { return externalLBAddress(k) },
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("app controller: %w", err)
	}
	drains := &controller.LogDrainReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorderFor("shpyrd"), SystemNamespace: k.Namespace}
	if err := drains.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("log drain controller: %w", err)
	}
	volumes := &controller.VolumeReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorderFor("shpyrd"),
		// Profile storage (RFC-0060).
		DefaultClass: os.Getenv(install.VarStorageClass), SharedClass: os.Getenv(install.VarStorageClassShared), SnapshotClass: os.Getenv(install.VarSnapshotClass),
	}
	if err := volumes.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("volume controller: %w", err)
	}
	memberships.Client, memberships.Scheme = mgr.GetClient(), mgr.GetScheme()
	if err := memberships.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("membership controller: %w", err)
	}
	// Front doors of explicit workspaces (RFC-0033 phase 6); nothing to do
	// while there is one workspace.
	workspaces := &controller.WorkspaceReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Store: memberships.Store, Config: rec.Config}
	if err := workspaces.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("workspace controller: %w", err)
	}
	if err := mgr.Add(workspaces); err != nil {
		return nil, fmt.Errorf("workspace controller: %w", err)
	}
	deps := ext.Deps{Kube: k, Client: mgr.GetClient(), SystemNamespace: k.Namespace, Vars: os.Getenv}
	for _, x := range enabledExts {
		if err := x.Register(mgr, deps); err != nil {
			return nil, fmt.Errorf("extension %s: %w", x.Name(), err)
		}
	}
	return mgr, nil
}

// externalLBAddress reads the public front door's address, what a custom
// domain's A record points at (RFC-0034): the static addresses the profile
// reserved (SHPYRD_LB_IP, comma-separated on AWS), else the ingress-nginx
// Service's.
func externalLBAddress(k *kube.Client) string {
	if fixed := os.Getenv(install.VarLBIP); fixed != "" {
		return fixed
	}
	svc, err := k.Kube.CoreV1().Services("ingress-nginx").Get(
		context.Background(), "ingress-nginx-controller", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			return in.IP
		}
		if in.Hostname != "" {
			return in.Hostname
		}
	}
	return ""
}

// internalLBAddress reads the address (or hostname) of the internal
// ingress-nginx Service at startup; the controller uses it as the
// ExternalDNS target for Ingresses with exposure:internal so their records
// point at the private LB.
func internalLBAddress(k *kube.Client) string {
	svc, err := k.Kube.CoreV1().Services("ingress-nginx-internal").Get(
		context.Background(), "ingress-nginx-internal-controller", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			return in.IP
		}
		if in.Hostname != "" {
			return in.Hostname // an NLB on AWS: ExternalDNS makes an alias of it
		}
	}
	return ""
}

// gcOrNil keeps a nil *RegistryGC from becoming a non-nil interface.
func gcOrNil(g *controller.RegistryGC) api.RegistryGC {
	if g == nil {
		return nil
	}
	return g
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

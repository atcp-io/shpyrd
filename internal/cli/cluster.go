package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"shpyrd/pkg/install"
	"shpyrd/pkg/kind"
	"shpyrd/pkg/kube"
	"shpyrd/pkg/localca"
)

const (
	defaultClusterName = "shpyrd"
	defaultDomain      = "127.0.0.1.nip.io"
)

// initFlags are shared by `cluster create` and `cluster init`.
type initFlags struct {
	profile   string
	domain    string
	set       []string
	skip      []string
	only      []string
	yes       bool
	httpPort  int
	httpsPort int
}

func (f *initFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.profile, "profile", "local", "installer profile (local)")
	cmd.Flags().StringVar(&f.domain, "domain", defaultDomain, "wildcard domain for projects and the dashboard")
	cmd.Flags().StringArrayVar(&f.set, "set", nil, "override a variable, e.g. --set SHPYRD_REGISTRY_HOST=...")
	cmd.Flags().StringSliceVar(&f.skip, "skip", nil, "components to skip, e.g. --skip monitoring")
	cmd.Flags().StringSliceVar(&f.only, "only", nil, "apply only these components")
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "do not ask for confirmation")
}

func (f *initFlags) vars(clusterName string) (map[string]string, error) {
	vars := map[string]string{
		install.VarDomain:  f.domain,
		install.VarCluster: clusterName,
	}
	if f.httpPort != 0 {
		vars[install.VarHTTPPort] = strconv.Itoa(f.httpPort)
	}
	if f.httpsPort != 0 {
		vars[install.VarHTTPSPort] = strconv.Itoa(f.httpsPort)
	}
	for _, kv := range f.set {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, "SHPYRD_") {
			return nil, fmt.Errorf("--set expects SHPYRD_NAME=value, got %q", kv)
		}
		vars[k] = v
	}
	return vars, nil
}

func newClusterCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Create, bootstrap and inspect shpyrd clusters",
	}
	cmd.AddCommand(
		newClusterCreateCmd(g),
		newClusterInitCmd(g),
		newClusterStatusCmd(g),
		newClusterDestroyCmd(g),
		newClusterTrustCACmd(),
		newClusterExportCmd(),
		newClusterTokenCmd(g),
		newClusterDashboardCmd(g),
	)
	return cmd
}

// adminToken reads the dashboard token from the cluster.
func adminToken(ctx context.Context, k *kube.Client) (string, error) {
	sec, err := k.Kube.CoreV1().Secrets(install.DefaultSystemNamespace).Get(ctx, install.AdminTokenSecretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read admin token: %w (is the shpyrd component installed?)", err)
	}
	tok := strings.TrimSpace(string(sec.Data["token"]))
	if tok == "" {
		return "", fmt.Errorf("admin token secret is empty")
	}
	return tok, nil
}

func newClusterTokenCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "token",
		Short: "Print the dashboard/API admin token",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			tok, err := adminToken(ctx, k)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), tok)
			return nil
		},
	}
}

func newClusterDashboardCmd(g *globalFlags) *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Open the shpyrd dashboard in the browser (logs you in with the admin token)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			info, err := install.ReadInstallInfo(ctx, k, "")
			if err != nil {
				return fmt.Errorf("shpyrd is not installed on this cluster (%v)", err)
			}
			tok, err := adminToken(ctx, k)
			if err != nil {
				return err
			}
			url := install.BaseURL(info.Vars)("shpyrd")
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Dashboard: %s\nToken:     %s\n", url, tok)
			if noOpen {
				return nil
			}
			// The token travels in the URL fragment, which browsers never send
			// to the server; the dashboard stores it locally and drops it from
			// the address bar.
			return openBrowser(url + "/#token=" + tok)
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "only print the URL and token")
	return cmd
}

func newClusterCreateCmd(g *globalFlags) *cobra.Command {
	var (
		name      string
		workers   int
		image     string
		noInit    bool
		httpPort  int
		httpsPort int
		flags     initFlags
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a local kind cluster and install the shpyrd base stack",
		Long: `Create a local Kubernetes cluster with kind (Docker is required) and install
the shpyrd base stack on it: cert-manager with a development CA, ingress-nginx,
an image registry, kpack, monitoring and the shpyrd server.

Host ports 80 and 443 are mapped to the cluster so apps are reachable at
https://<app>.<domain>; the default domain 127.0.0.1.nip.io resolves to the
local machine without any configuration.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			p := kind.NewProvider()
			exists, err := p.Exists(name)
			if err != nil {
				return err
			}
			if exists {
				fmt.Fprintf(cmd.OutOrStdout(), "kind cluster %q already exists, skipping creation\n", name)
			} else {
				if err := checkPortsFree(httpPort, httpsPort, 30050); err != nil {
					return err
				}
				cfg := kind.Defaults(name)
				cfg.Workers = workers
				cfg.NodeImage = image
				cfg.HTTPPort = httpPort
				cfg.HTTPSPort = httpsPort
				cfg.KubeconfigPath = g.kubeconfig
				fmt.Fprintf(cmd.OutOrStdout(), "Creating kind cluster %q (%d worker(s), host ports %d/%d)...\n", name, workers, httpPort, httpsPort)
				if err := p.Create(cfg); err != nil {
					return err
				}
			}
			flags.httpPort, flags.httpsPort = httpPort, httpsPort
			if noInit {
				fmt.Fprintf(cmd.OutOrStdout(), "Cluster ready. Run `shpyrd cluster init --context %s` to install the base stack.\n", kind.ContextName(name))
				return nil
			}
			return runInit(ctx, cmd, kube.Options{Kubeconfig: g.kubeconfig, Context: kind.ContextName(name)}, name, &flags, true)
		},
	}
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "kind cluster name")
	cmd.Flags().IntVar(&workers, "workers", 1, "number of worker nodes")
	cmd.Flags().StringVar(&image, "image", "", "kind node image (defaults to the kind release default)")
	cmd.Flags().BoolVar(&noInit, "no-init", false, "only create the cluster, do not install the base stack")
	cmd.Flags().IntVar(&httpPort, "http-port", 80, "host port forwarded to ingress HTTP")
	cmd.Flags().IntVar(&httpsPort, "https-port", 443, "host port forwarded to ingress HTTPS")
	flags.bind(cmd)
	return cmd
}

// checkPortsFree fails early when a host port kind needs is already bound,
// which would otherwise surface as an opaque `docker run` exit status 125.
func checkPortsFree(ports ...int) error {
	var busy []string
	for _, p := range ports {
		if p == 0 {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			busy = append(busy, strconv.Itoa(p))
			continue
		}
		_ = l.Close()
	}
	if len(busy) > 0 {
		return fmt.Errorf("host port(s) %s already in use; stop the process using them or pass --http-port/--https-port (e.g. --http-port 8080 --https-port 8443)", strings.Join(busy, ", "))
	}
	return nil
}

func newClusterInitCmd(g *globalFlags) *cobra.Command {
	var (
		name  string
		flags initFlags
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install or upgrade the shpyrd base stack on the current cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			return runInit(ctx, cmd, kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx}, name, &flags, false)
		},
	}
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "logical cluster name (SHPYRD_CLUSTER)")
	cmd.Flags().IntVar(&flags.httpPort, "http-port", 0, "host port that reaches ingress HTTP (recorded for URLs)")
	cmd.Flags().IntVar(&flags.httpsPort, "https-port", 0, "host port that reaches ingress HTTPS (recorded for URLs)")
	flags.bind(cmd)
	return cmd
}

func runInit(ctx context.Context, cmd *cobra.Command, kopts kube.Options, clusterName string, flags *initFlags, fromCreate bool) error {
	out := cmd.OutOrStdout()
	rawCfg, err := kopts.ClientConfig().RawConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	contextName := kopts.Context
	if contextName == "" {
		contextName = rawCfg.CurrentContext
	}
	if contextName == "" {
		return fmt.Errorf("no kubeconfig context selected; use --context or create a cluster with `shpyrd cluster create`")
	}
	if !fromCreate && !flags.yes && !strings.HasPrefix(contextName, "kind-") {
		return fmt.Errorf("context %q does not look like a local kind cluster; re-run with --yes to install the base stack on it", contextName)
	}

	k, err := kube.Connect(kopts)
	if err != nil {
		return err
	}
	vars, err := flags.vars(clusterName)
	if err != nil {
		return err
	}
	eng, err := install.New(k, install.Options{
		Profile:  flags.profile,
		Vars:     vars,
		Skip:     flags.skip,
		Only:     flags.only,
		Version:  Version,
		Reporter: &consoleReporter{out: out},
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Installing shpyrd base stack (profile %s) on context %s\n", eng.Profile().Name, contextName)
	fmt.Fprintf(out, "Domain: %s\n", eng.Vars()[install.VarDomain])
	if err := eng.Apply(ctx); err != nil {
		return err
	}

	base := install.BaseURL(eng.Vars())
	fmt.Fprintf(out, "\nshpyrd is ready.\n\n")
	fmt.Fprintf(out, "  Dashboard:  %s\n", base("shpyrd"))
	fmt.Fprintf(out, "  Grafana:    %s\n", base("grafana"))
	fmt.Fprintf(out, "  Registry:   %s (host: localhost:30050)\n", eng.Vars()[install.VarRegistryHost])
	if caDir, err := localca.DefaultDir(); err == nil {
		fmt.Fprintf(out, "  Root CA:    %s/rootCA.pem\n", caDir)
	}
	fmt.Fprintf(out, "\nRun `shpyrd cluster trust-ca` once so your browser trusts the development CA.\n")
	return nil
}

func newClusterStatusCmd(g *globalFlags) *cobra.Command {
	var profile string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the health of the base stack components",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			info, err := install.ReadInstallInfo(ctx, k, "")
			if err != nil {
				return fmt.Errorf("shpyrd is not installed on this cluster (%v)", err)
			}
			if profile == "" {
				profile = info.Profile
			}
			eng, err := install.New(k, install.Options{Profile: profile, Vars: info.Vars, Version: info.Version, Reporter: &quietReporter{}})
			if err != nil {
				return err
			}
			statuses, err := eng.Status(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Profile: %s  Version: %s  Domain: %s  Updated: %s\n\n", info.Profile, info.Version, info.Vars[install.VarDomain], info.UpdatedAt)
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "RUNLEVEL\tCOMPONENT\tSTATUS\tVERSION\tAPPLIED")
			allReady := true
			for _, s := range statuses {
				status := "ready"
				if !s.Ready {
					allReady = false
					status = "not ready"
					if s.Detail != "" {
						status += " (" + s.Detail + ")"
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Runlevel, s.Name, status, s.Version, s.AppliedAt)
			}
			tw.Flush()
			if !allReady {
				return fmt.Errorf("some components are not ready")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "profile to evaluate (defaults to the installed one)")
	return cmd
}

func newClusterDestroyCmd(g *globalFlags) *cobra.Command {
	var (
		name string
		yes  bool
	)
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Delete the local kind cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			p := kind.NewProvider()
			exists, err := p.Exists(name)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("kind cluster %q does not exist", name)
			}
			if !yes {
				fmt.Fprintf(cmd.OutOrStdout(), "Delete kind cluster %q and everything in it? [y/N] ", name)
				var answer string
				fmt.Fscanln(os.Stdin, &answer)
				if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
					return fmt.Errorf("aborted")
				}
			}
			return p.Delete(name, g.kubeconfig)
		},
	}
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "kind cluster name")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func newClusterTrustCACmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "trust-ca",
		Short: "Install the development root CA in the operating system trust store",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if dir == "" {
				var err error
				dir, err = localca.DefaultDir()
				if err != nil {
					return err
				}
			}
			ca, created, err := localca.LoadOrCreate(dir)
			if err != nil {
				return err
			}
			if created {
				fmt.Fprintf(out, "Generated development root CA at %s\n", ca.CertPath())
			}
			fmt.Fprintf(out, "Installing %s into the system trust store (administrator rights required)...\n", ca.CertPath())
			res, err := ca.Trust()
			if err != nil {
				fmt.Fprintln(out, res.Instructions)
				return err
			}
			if res.Installed {
				fmt.Fprintln(out, "Development CA trusted.")
			}
			if res.Instructions != "" {
				fmt.Fprintln(out, res.Instructions)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "ca-dir", "", "directory of the development CA (default ~/.shpyrd/ca)")
	return cmd
}

func newClusterExportCmd() *cobra.Command {
	var (
		outDir string
		name   string
		flags  initFlags
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Render the base stack manifests to a directory (for GitOps tooling)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			vars, err := flags.vars(name)
			if err != nil {
				return err
			}
			eng, err := install.New(nil, install.Options{
				Profile:  flags.profile,
				Vars:     vars,
				Skip:     flags.skip,
				Only:     flags.only,
				Version:  Version,
				Reporter: &consoleReporter{out: cmd.OutOrStdout()},
			})
			if err != nil {
				return err
			}
			if err := eng.Export(ctx, outDir); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nExported to %s\n", outDir)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outDir, "out", "o", "./shpyrd-export", "output directory")
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "logical cluster name (SHPYRD_CLUSTER)")
	flags.bind(cmd)
	return cmd
}

// quietReporter discards progress (used by status).
type quietReporter struct{}

func (quietReporter) Runlevel(string, []string)  {}
func (quietReporter) Step(string, string)        {}
func (quietReporter) Done(string, time.Duration) {}
func (quietReporter) Failed(string, error)       {}

func signalContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx
}

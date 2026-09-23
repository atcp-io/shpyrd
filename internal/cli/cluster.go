package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/api"
	"shpyrd/pkg/audit"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kind"
	"shpyrd/pkg/kube"
	"shpyrd/pkg/localca"
	"shpyrd/pkg/localnet"
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
	enable    []string
	disable   []string
	yes       bool
	httpPort  int
	httpsPort int
	// Local names and front door (RFC-0057).
	frontDoor string // auto, kind or caddy
	localDNS  bool
	caddy     *localnet.Caddy
}

func (f *initFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.profile, "profile", "local", "installer profile (local)")
	cmd.Flags().StringVar(&f.domain, "domain", defaultDomain, "wildcard domain for projects and the dashboard (shpyrd.test with --local-dns)")
	cmd.Flags().StringVar(&f.frontDoor, "front-door", frontDoorAuto, "who serves 443: kind (host ports), caddy (an existing Caddy proxies to kind), or auto (detect and ask)")
	cmd.Flags().BoolVar(&f.localDNS, "local-dns", false, "make *.<domain> resolve to this machine with dnsmasq and /etc/resolver (macOS)")
	cmd.Flags().StringArrayVar(&f.set, "set", nil, "override a variable, e.g. --set SHPYRD_REGISTRY_HOST=...")
	cmd.Flags().StringSliceVar(&f.skip, "skip", nil, "components to skip, e.g. --skip monitoring")
	cmd.Flags().StringSliceVar(&f.only, "only", nil, "apply only these components")
	cmd.Flags().StringSliceVar(&f.enable, "enable", nil, "extensions to enable, e.g. --enable auth-local (see `shpyrd extensions list`)")
	cmd.Flags().StringSliceVar(&f.disable, "disable", nil, "extensions to stop installing (their components are left in place; use `shpyrd extensions disable` to remove them)")
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
	if f.frontDoor != "" && f.frontDoor != frontDoorAuto {
		vars[install.VarFrontDoor] = f.frontDoor
	}
	vars[install.VarLocalDNS] = strconv.FormatBool(f.localDNS)
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
	var (
		rotate  bool
		disable bool
		enable  bool
	)
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print the dashboard/API admin token (--rotate replaces it, --disable switches it off)",
		Long: `The admin token is a shared credential with full platform-admin rights,
meant for bootstrap and automation. Once accounts exist (extension
auth-local or another provider) and a platform-admin team includes at least
one person, --disable switches it off: the API refuses it and sign-in goes
through accounts only. --enable turns it back on; --rotate replaces it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			if disable || enable {
				if disable && enable {
					return errors.New("--disable and --enable are exclusive")
				}
				if disable {
					if err := checkTokenCanBeDisabled(ctx, k); err != nil {
						return err
					}
				}
				if err := setTokenDisabled(ctx, k, disable); err != nil {
					return err
				}
				if disable {
					fmt.Fprintln(cmd.OutOrStdout(), "Admin token disabled; the server is restarting. Sign in with your account; `shpyrd cluster dashboard` still works through one-time tickets.")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "Admin token enabled; the server is restarting.")
				}
				return nil
			}
			if rotate {
				tok, err := rotateAdminToken(ctx, k)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.ErrOrStderr(), "Admin token rotated; the server is restarting with it. Automation using the old token must be updated:")
				fmt.Fprintln(cmd.OutOrStdout(), tok)
				return nil
			}
			tok, err := adminToken(ctx, k)
			if err != nil {
				return err
			}
			if disabled, _ := tokenDisabled(ctx, k); disabled {
				fmt.Fprintln(cmd.ErrOrStderr(), "Note: the admin token is disabled on this cluster (`shpyrd cluster token --enable` turns it back on).")
			}
			fmt.Fprintln(cmd.OutOrStdout(), tok)
			return nil
		},
	}
	cmd.Flags().BoolVar(&rotate, "rotate", false, "generate a new token, store it and restart the server")
	cmd.Flags().BoolVar(&disable, "disable", false, "switch the token off (needs a login provider and a platform-admin team)")
	cmd.Flags().BoolVar(&enable, "enable", false, "switch the token back on")
	return cmd
}

// tokenDisabled reads the switch from the token Secret.
func tokenDisabled(ctx context.Context, k *kube.Client) (bool, error) {
	sec, err := k.Kube.CoreV1().Secrets(install.DefaultSystemNamespace).Get(ctx, install.AdminTokenSecretName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return string(sec.Data["disabled"]) == "true", nil
}

// checkTokenCanBeDisabled refuses to lock everyone out: accounts must be
// able to sign in and someone must be a platform admin.
func checkTokenCanBeDisabled(ctx context.Context, k *kube.Client) error {
	exts := recordedExtensions(ctx, k)
	hasProvider := false
	for _, e := range exts {
		if strings.HasPrefix(e, "auth-") {
			hasProvider = true
		}
	}
	if !hasProvider {
		return errors.New("no login provider is enabled: enable one first (`shpyrd extensions enable auth-local`) or nobody could sign in")
	}
	c, err := k.ControllerClient()
	if err != nil {
		return err
	}
	var teams shpyrdv1.TeamList
	if err := c.List(ctx, &teams); err != nil {
		return err
	}
	for _, t := range teams.Items {
		if t.Spec.PlatformRole == shpyrdv1.RolePlatformAdmin && (len(t.Spec.Members) > 0 || len(t.Spec.Groups) > 0) {
			return nil
		}
	}
	return errors.New("no team holds the platform-admin role: create one that includes you first (`shpyrd teams create platform --platform-role platform-admin --member you@example.com`)")
}

// setTokenDisabled flips the switch and restarts the server.
func setTokenDisabled(ctx context.Context, k *kube.Client, disabled bool) error {
	secrets := k.Kube.CoreV1().Secrets(install.DefaultSystemNamespace)
	sec, err := secrets.Get(ctx, install.AdminTokenSecretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read admin token: %w", err)
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	if disabled {
		sec.Data["disabled"] = []byte("true")
	} else {
		delete(sec.Data, "disabled")
	}
	sec.StringData = nil
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update admin token: %w", err)
	}
	return restartServer(ctx, k)
}

// restartServer rolls the server deployment so it re-reads its Secrets.
func restartServer(ctx context.Context, k *kube.Client) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"shpyrd.io/restarted-at":%q}}}}}`, time.Now().UTC().Format(time.RFC3339))
	if _, err := k.Kube.AppsV1().Deployments(install.DefaultSystemNamespace).Patch(ctx, "shpyrd-server", types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("restart server: %w", err)
	}
	return nil
}

// rotateAdminToken replaces the token Secret and restarts the server so it
// picks the new value up (RFC-0008).
func rotateAdminToken(ctx context.Context, k *kube.Client) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	secrets := k.Kube.CoreV1().Secrets(install.DefaultSystemNamespace)
	sec, err := secrets.Get(ctx, install.AdminTokenSecretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read admin token: %w", err)
	}
	sec.Data = map[string][]byte{"token": []byte(tok)}
	sec.StringData = nil
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return "", fmt.Errorf("update admin token: %w", err)
	}
	if err := restartServer(ctx, k); err != nil {
		return "", err
	}
	return tok, nil
}

func newClusterDashboardCmd(g *globalFlags) *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Open the shpyrd dashboard in the browser, signed in as you",
		Long: `Open the dashboard signed in through a one-time login ticket: a short-lived
Secret in the cluster that the browser redeems for a normal session. The admin
token never reaches the browser, and the session is attributed to you
(user@host) in the audit trail. The ticket is valid for 60 seconds and once.`,
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
			code, err := api.MintLoginTicket(ctx, k.Kube, install.DefaultSystemNamespace, audit.LocalActor())
			if err != nil {
				return err
			}
			base := install.BaseURL(info.Vars)("shpyrd")
			login := base + "/api/auth/ticket?code=" + url.QueryEscape(code)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Dashboard: %s\n", base)
			if noOpen {
				fmt.Fprintf(out, "Sign-in:   %s\n           (one-time link, valid for %s)\n", login, api.LoginTicketTTL)
				return nil
			}
			fmt.Fprintln(out, "Opening the dashboard signed in as", audit.LocalActor())
			return openBrowser(login)
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "only print the URL and the one-time sign-in link")
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
local machine without any configuration. When a Caddy already serves 443,
it can be the front door instead (--front-door caddy): kind takes high ports,
Caddy proxies *.<domain> to it, URLs carry no port and certificates come from
Caddy's CA. With --local-dns (or when Caddy is the front door), *.shpyrd.test
resolves to this machine through dnsmasq.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			p := kind.NewProvider()
			exists, err := p.Exists(name)
			if err != nil {
				return err
			}
			flags.httpPort, flags.httpsPort = httpPort, httpsPort
			if exists {
				fmt.Fprintf(cmd.OutOrStdout(), "kind cluster %q already exists, skipping creation\n", name)
				if err := seedFromRecord(ctx, cmd, kube.Options{Kubeconfig: g.kubeconfig, Context: kind.ContextName(name)}, &flags); err != nil {
					return err
				}
			} else {
				if err := planLocal(cmd, &flags, cmd.Flags().Changed("domain"), cmd.Flags().Changed("http-port") || cmd.Flags().Changed("https-port")); err != nil {
					return err
				}
				if err := checkPortsFree(flags.httpPort, flags.httpsPort, 30050); err != nil {
					return err
				}
				if err := ensureLocalDNS(cmd, &flags); err != nil {
					return err
				}
				httpPort, httpsPort = flags.httpPort, flags.httpsPort
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
			kopts := kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx}
			if err := seedFromRecord(ctx, cmd, kopts, &flags); err != nil {
				return err
			}
			if flags.frontDoor == install.FrontDoorCaddy && flags.httpPort == 0 {
				return errors.New("--front-door caddy needs the host port kind maps to ingress HTTP: pass --http-port")
			}
			if flags.frontDoor == frontDoorAuto {
				flags.frontDoor = install.FrontDoorKind
			}
			if cmd.Flags().Changed("local-dns") || cmd.Flags().Changed("domain") {
				if err := ensureLocalDNS(cmd, &flags); err != nil {
					return err
				}
			}
			return runInit(ctx, cmd, kopts, name, &flags, false)
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
	// Extensions already enabled on the cluster stay enabled.
	extNames := mergeExtensions(recordedExtensions(ctx, k), flags.enable, flags.disable)
	extComps, err := extensionComponents(extNames)
	if err != nil {
		return err
	}
	eng, err := install.New(k, install.Options{
		Profile:    flags.profile,
		Vars:       vars,
		Skip:       flags.skip,
		Only:       flags.only,
		Version:    Version,
		Extensions: extComps,
		Reporter:   &consoleReporter{out: out},
	})
	if err != nil {
		return err
	}
	if len(extNames) > 0 {
		fmt.Fprintf(out, "Extensions: %s\n", strings.Join(extNames, ", "))
	}

	fmt.Fprintf(out, "Installing shpyrd base stack (profile %s) on context %s\n", eng.Profile().Name, contextName)
	fmt.Fprintf(out, "Domain: %s\n", eng.Vars()[install.VarDomain])
	if err := eng.Apply(ctx); err != nil {
		return err
	}

	if flags.frontDoor == install.FrontDoorCaddy {
		if err := setupFrontDoor(ctx, cmd, flags); err != nil {
			return err
		}
	}

	base := install.BaseURL(eng.Vars())
	fmt.Fprintf(out, "\nshpyrd is ready.\n\n")
	fmt.Fprintf(out, "  Dashboard:  %s\n", base("shpyrd"))
	fmt.Fprintf(out, "  Grafana:    %s\n", base("grafana"))
	fmt.Fprintf(out, "  Registry:   %s (host: localhost:30050)\n", eng.Vars()[install.VarRegistryHost])
	if caDir, err := localca.DefaultDir(); err == nil && flags.frontDoor != install.FrontDoorCaddy {
		fmt.Fprintf(out, "  Root CA:    %s/rootCA.pem\n", caDir)
	}
	printLocalSummary(out, eng.Vars())
	return nil
}

// seedFromRecord fills flags the user did not pass from the install record,
// so `cluster init` keeps the domain, ports and front door the cluster was
// created with.
func seedFromRecord(ctx context.Context, cmd *cobra.Command, kopts kube.Options, flags *initFlags) error {
	k, err := kube.Connect(kopts)
	if err != nil {
		return nil // runInit reports connection problems
	}
	info, err := install.ReadInstallInfo(ctx, k, "")
	if err != nil || info == nil {
		return nil // first install
	}
	f := cmd.Flags()
	if !f.Changed("domain") && info.Vars[install.VarDomain] != "" {
		flags.domain = info.Vars[install.VarDomain]
	}
	if !f.Changed("http-port") && flags.httpPort == 0 {
		flags.httpPort, _ = strconv.Atoi(info.Vars[install.VarHTTPPort])
	}
	if !f.Changed("https-port") && flags.httpsPort == 0 {
		flags.httpsPort, _ = strconv.Atoi(info.Vars[install.VarHTTPSPort])
	}
	if !f.Changed("front-door") && info.Vars[install.VarFrontDoor] != "" {
		flags.frontDoor = info.Vars[install.VarFrontDoor]
	}
	if !f.Changed("local-dns") {
		flags.localDNS = info.Vars[install.VarLocalDNS] == "true"
	}
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
			extComps, err := extensionComponents(splitList(info.Vars[install.VarExtensions]))
			if err != nil {
				return err
			}
			eng, err := install.New(k, install.Options{Profile: profile, Vars: info.Vars, Version: info.Version, Extensions: extComps, Reporter: &quietReporter{}})
			if err != nil {
				return err
			}
			statuses, err := eng.Status(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Profile: %s  Version: %s  Domain: %s  Updated: %s\n", info.Profile, info.Version, info.Vars[install.VarDomain], info.UpdatedAt)
			fmt.Fprintf(out, "%s\n\n", describeLocal(info.Vars))
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
			if !confirm(cmd, yes, fmt.Sprintf("Delete kind cluster %q and everything in it?", name), false) {
				return fmt.Errorf("aborted")
			}
			// The record dies with the cluster: read it first to know what
			// was written on this machine (RFC-0057).
			var domain, frontDoor string
			var localDNS bool
			if k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: kind.ContextName(name)}); err == nil {
				if info, err := install.ReadInstallInfo(signalContext(), k, ""); err == nil && info != nil {
					domain, frontDoor = info.Vars[install.VarDomain], info.Vars[install.VarFrontDoor]
					localDNS = info.Vars[install.VarLocalDNS] == "true"
				}
			}
			if err := p.Delete(name, g.kubeconfig); err != nil {
				return err
			}
			teardownLocal(signalContext(), cmd, yes, domain, frontDoor, localDNS)
			return nil
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

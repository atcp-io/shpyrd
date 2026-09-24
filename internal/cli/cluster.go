package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/deploy"
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
	// An external registry instead of the in-cluster one (RFC-0059); its
	// credentials are written to a Secret by the registry-credentials hook,
	// never to the install record.
	registryHost      string
	registryUser      string
	registryTokenFile string
	// DNS automation (RFC-0061): provider, where the zone lives and how the
	// cluster authenticates; the key goes to a Secret, never to the record.
	dns            string
	dnsAuth        string
	dnsCompartment string
	dnsTenancy     string
	dnsRegion      string
	dnsUser        string
	dnsKeyFile     string
	dnsFingerprint string
	// Front doors (RFC-0036).
	internalLBSubnet string // OCI subnet OCID for the private LB
	platformExposure string // "" = profile default
}

func (f *initFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.profile, "profile", "local", "installer profile: local (kind) or oci (Oracle OKE)")
	cmd.Flags().StringVar(&f.domain, "domain", defaultDomain, "wildcard domain for projects and the dashboard (shpyrd.test with --local-dns)")
	cmd.Flags().StringVar(&f.frontDoor, "front-door", frontDoorAuto, "who serves 443: kind (host ports), caddy (an existing Caddy proxies to kind), or auto (detect and ask)")
	cmd.Flags().BoolVar(&f.localDNS, "local-dns", false, "make *.<domain> resolve to this machine with dnsmasq and /etc/resolver (macOS)")
	cmd.Flags().StringVar(&f.registryHost, "registry-host", "", "use this registry instead of the in-cluster one, e.g. gru.ocir.io/<tenancy-namespace> (with --registry-user and --registry-token-file)")
	cmd.Flags().StringVar(&f.registryUser, "registry-user", "", "user of the external registry, e.g. <tenancy-namespace>/<user> for OCIR")
	cmd.Flags().StringVar(&f.registryTokenFile, "registry-token-file", "", "file holding the external registry's password or auth token")
	cmd.Flags().StringVar(&f.internalLBSubnet, "internal-lb-subnet", "", "subnet OCID for the internal load balancer (OCI, RFC-0036); an empty value with SHPYRD_INTERNAL_LB=auto skips the internal controller")
	cmd.Flags().StringVar(&f.platformExposure, "platform-exposure", "", "whether the platform dashboard uses the external or internal front door (external|internal)")
	cmd.Flags().StringVar(&f.dns, "dns", "", "DNS provider that hosts the platform's zone: oci (records and the wildcard certificate are then automatic) or none")
	cmd.Flags().StringVar(&f.dnsAuth, "dns-auth", "", "how the cluster authenticates to the DNS provider: key (default with --dns-key-file) or workload (OKE workload identity, enhanced clusters)")
	cmd.Flags().StringVar(&f.dnsCompartment, "dns-compartment", "", "OCI compartment OCID holding the zone")
	cmd.Flags().StringVar(&f.dnsTenancy, "dns-tenancy", "", "OCI tenancy OCID (with --dns-key-file)")
	cmd.Flags().StringVar(&f.dnsRegion, "dns-region", "", "OCI region of the zone, e.g. sa-saopaulo-1")
	cmd.Flags().StringVar(&f.dnsUser, "dns-user", "", "OCI user OCID owning the API key (with --dns-key-file)")
	cmd.Flags().StringVar(&f.dnsKeyFile, "dns-key-file", "", "PEM file with the DNS user's API signing key (contrib/oci/terraform writes it)")
	cmd.Flags().StringVar(&f.dnsFingerprint, "dns-fingerprint", "", "fingerprint of the API key (derived from the key when omitted)")
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
	if f.registryHost != "" {
		// External registry: no in-cluster address, TLS from a public CA.
		vars[install.VarRegistryHost] = f.registryHost
		vars[install.VarRegistryIP] = ""
		vars[install.VarRegistryInsecure] = "false"
	}
	if f.dns != "" {
		vars[install.VarDNSProvider] = f.dns
	}
	if f.dnsAuth != "" {
		vars[install.VarDNSAuth] = f.dnsAuth
	} else if f.dnsKeyFile != "" {
		vars[install.VarDNSAuth] = install.DNSAuthKey
	}
	for k, v := range map[string]string{
		install.VarDNSCompartment: f.dnsCompartment,
		install.VarDNSTenancy:     f.dnsTenancy,
		install.VarDNSRegion:      f.dnsRegion,
		install.VarDNSUser:        f.dnsUser,
	} {
		if v != "" {
			vars[k] = v
		}
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
		newClusterTrustCACmd(g),
		newClusterExportCmd(),
		newClusterTokenCmd(g),
		newClusterDashboardCmd(g),
		newClusterRegistryCmd(g),
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
			local := flags.profile == "local"
			if flags.frontDoor == frontDoorAuto && local {
				flags.frontDoor = install.FrontDoorKind // cloud profiles keep their own (lb)
			}
			if local && (cmd.Flags().Changed("local-dns") || cmd.Flags().Changed("domain")) {
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
	if !fromCreate && !flags.yes && flags.profile == "local" && !strings.HasPrefix(contextName, "kind-") {
		return fmt.Errorf("context %q does not look like a local kind cluster; pass --profile oci for a cloud cluster, or --yes to install the local profile on it", contextName)
	}
	registryPassword := ""
	if flags.registryTokenFile != "" {
		raw, err := os.ReadFile(flags.registryTokenFile)
		if err != nil {
			return fmt.Errorf("registry token: %w", err)
		}
		registryPassword = strings.TrimSpace(string(raw))
	}
	if (flags.registryUser == "") != (registryPassword == "") {
		return errors.New("--registry-user and --registry-token-file go together")
	}
	if flags.registryHost != "" && flags.registryUser == "" && len(flags.only) == 0 {
		return errors.New("--registry-host needs --registry-user and --registry-token-file")
	}
	dnsKey := ""
	if flags.dnsKeyFile != "" {
		raw, err := os.ReadFile(flags.dnsKeyFile)
		if err != nil {
			return fmt.Errorf("DNS key: %w", err)
		}
		dnsKey = string(raw)
	}
	if flags.dns != "" && flags.dns != "none" && flags.dns != "oci" {
		return fmt.Errorf("--dns %q: oci or none", flags.dns)
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
	skip := flags.skip
	if ip, explicit := vars[install.VarRegistryIP]; explicit && ip == "" {
		// An external registry (--registry-host now, or recorded from an
		// earlier run) replaces the in-cluster one and its node trust.
		skip = append(append([]string{}, skip...), "registry", "registry-nodes")
	}
	if prof, err := install.LoadProfile(deploy.FS, flags.profile); err == nil {
		if vars[install.VarNetworkPolicy] == "none" && prof.HasComponent("network-policy") {
			skip = append(append([]string{}, skip...), "network-policy")
		}
		// Shared volumes need the File Storage mount target (RFC-0060); the
		// class makes no sense without it and the Volume controller explains.
		if effectiveVar(vars, prof, install.VarFSSMountTarget) == "" && prof.HasComponent("storage-fss") {
			skip = append(append([]string{}, skip...), "storage-fss")
		}
		// No DNS provider: no records automation, no wildcard certificate.
		if dns := effectiveVar(vars, prof, install.VarDNSProvider); dns == "" || dns == "none" {
			for _, c := range []string{"external-dns", "dns01-oci", "dns"} {
				if prof.HasComponent(c) {
					skip = append(append([]string{}, skip...), c)
				}
			}
		}
	}
	opts := install.Options{
		Profile:           flags.profile,
		Vars:              vars,
		Skip:              skip,
		Only:              flags.only,
		Version:           Version,
		Extensions:        extComps,
		Reporter:          &consoleReporter{out: out},
		RegistryUser:      flags.registryUser,
		RegistryPassword:  registryPassword,
		DNSKeyPEM:         dnsKey,
		DNSKeyFingerprint: flags.dnsFingerprint,
	}
	eng, err := install.New(k, opts)
	if err != nil {
		return err
	}
	if len(extNames) > 0 {
		fmt.Fprintf(out, "Extensions: %s\n", strings.Join(extNames, ", "))
	}

	fmt.Fprintf(out, "Installing shpyrd base stack (profile %s) on context %s\n", eng.Profile().Name, contextName)
	fmt.Fprintf(out, "Domain: %s\n", eng.Vars()[install.VarDomain])
	cloud := eng.Vars()[install.VarFrontDoor] == install.FrontDoorLB
	var lbAddress string
	if cloud && len(flags.only) == 0 {
		// Certificates need the wildcard DNS record, and the record needs
		// the load balancer's address: install everything that does not
		// wait for a certificate first, then let the operator create the
		// record, then the rest.
		first := install.Options(opts)
		first.Skip = append([]string{}, opts.Skip...)
		for _, c := range certificateComponents(extNames) {
			if eng.Profile().HasComponent(c) && !contains(first.Skip, c) {
				first.Skip = append(first.Skip, c)
			}
		}
		phase1, err := install.New(k, first)
		if err != nil {
			return err
		}
		if err := phase1.Apply(ctx); err != nil {
			return err
		}
		lbAddress, err = waitForLoadBalancer(ctx, cmd, k)
		if err != nil {
			return err
		}
		if dns := eng.Vars()[install.VarDNSProvider]; dns != "" && dns != "none" {
			fmt.Fprintf(out, "DNS: ExternalDNS publishes *.%s -> %s in the %s zone.\n", eng.Vars()[install.VarDomain], lbAddress, dns)
		}
		if err := waitForDNS(ctx, cmd, eng.Vars()[install.VarDomain], lbAddress); err != nil {
			return err
		}
	}
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
	if eng.Vars()[install.VarRegistryIP] != "" {
		fmt.Fprintf(out, "  Registry:   in-cluster at %s (TLS from the platform CA, credential in Secret %s)\n", eng.Vars()[install.VarRegistryHost], install.RegistrySecretName)
	} else {
		fmt.Fprintf(out, "  Registry:   %s (credentials in Secret %s)\n", eng.Vars()[install.VarRegistryHost], install.RegistrySecretName)
	}
	if cloud {
		if lbAddress == "" {
			lbAddress, _ = loadBalancerAddress(ctx, k)
		}
		intLBAddr := internalLBAddress(ctx, k)
		dns := eng.Vars()[install.VarDNSProvider]
		domain := eng.Vars()[install.VarDomain]
		if lbAddress != "" {
			if dns != "" && dns != "none" {
				fmt.Fprintf(out, "  External LB:   %s (ExternalDNS: *.%s)\n", lbAddress, domain)
			} else {
				fmt.Fprintf(out, "  External LB:   %s (DNS: *.%s -> %s)\n", lbAddress, domain, lbAddress)
			}
		}
		if intLBAddr != "" {
			if dns != "" && dns != "none" {
				fmt.Fprintf(out, "  Internal LB:   %s (ExternalDNS: per host, exposure:internal)\n", intLBAddr)
			} else {
				fmt.Fprintf(out, "  Internal LB:   %s (DNS: per host pointing here for exposure:internal projects)\n", intLBAddr)
			}
		}
	} else if caDir, err := localca.DefaultDir(); err == nil && flags.frontDoor != install.FrontDoorCaddy {
		fmt.Fprintf(out, "  Root CA:    %s/rootCA.pem\n", caDir)
	}
	printLocalSummary(out, eng.Vars())
	if engine := networkPolicyEngine(ctx, k); engine == "" {
		fmt.Fprintf(out, "\nWarning: no network policy engine found (Calico, Cilium, kindnet, Antrea, kube-router): the cluster's CNI does not enforce\nNetworkPolicy, so projects are not isolated from each other. On OKE with VCN-native pod networking the oci profile installs\nCalico in policy-only mode (SHPYRD_NETWORK_POLICY=calico).\n")
	}
	return nil
}

// networkPolicyEngine names the NetworkPolicy-enforcing agent running on the
// cluster's nodes, or "" when none is recognised. Kubernetes has no API for
// this: NetworkPolicy objects are accepted whether or not anything enforces
// them, so the known DaemonSets are the best signal.
func networkPolicyEngine(ctx context.Context, k *kube.Client) string {
	known := map[string]string{
		"calico-node":     "Calico",
		"cilium":          "Cilium",
		"kindnet":         "kindnet", // enforces policies since kind 0.24
		"antrea-agent":    "Antrea",
		"kube-router":     "kube-router",
		"weave-net":       "Weave Net",
		"canal":           "Canal",
		"aws-node":        "", // Amazon VPC CNI enforces only with its network policy agent alongside
		"kube-flannel":    "",
		"flannel":         "",
		"kube-flannel-ds": "",
	}
	for _, ns := range []string{"kube-system", "calico-system", "cilium", "tigera-operator"} {
		list, err := k.Kube.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, ds := range list.Items {
			if name, ok := known[ds.Name]; ok && name != "" {
				return name
			}
			if ds.Name == "aws-network-policy-agent" {
				return "Amazon VPC CNI network policy agent"
			}
		}
	}
	return ""
}

// certificateComponents are the components whose readiness needs a valid
// certificate, so they go last on cloud profiles: the server, and Dex when
// the auth-local extension is on.
func certificateComponents(extNames []string) []string {
	out := []string{"shpyrd", "dns"}
	if contains(extNames, "auth-local") {
		out = append(out, "dex")
	}
	return out
}

// effectiveVar is the value a variable will have: the explicit one, else
// the profile's default.
func effectiveVar(vars map[string]string, prof *install.Profile, key string) string {
	if v, ok := vars[key]; ok {
		return v
	}
	return prof.Vars[key]
}

// loadBalancerAddress is the public address of ingress-nginx's Service.
// internalLBAddress returns the address of the internal ingress controller
// Service, empty when none exists.
func internalLBAddress(ctx context.Context, k *kube.Client) string {
	svc, err := k.Kube.CoreV1().Services("ingress-nginx-internal").Get(ctx, "ingress-nginx-internal-controller", metav1.GetOptions{})
	if err != nil || len(svc.Status.LoadBalancer.Ingress) == 0 {
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

func loadBalancerAddress(ctx context.Context, k *kube.Client) (string, error) {
	svc, err := k.Kube.CoreV1().Services("ingress-nginx").Get(ctx, "ingress-nginx-controller", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			return in.IP, nil
		}
		if in.Hostname != "" {
			return in.Hostname, nil
		}
	}
	return "", nil
}

// waitForLoadBalancer polls until the cloud assigns ingress-nginx an address.
func waitForLoadBalancer(ctx context.Context, cmd *cobra.Command, k *kube.Client) (string, error) {
	out := cmd.OutOrStdout()
	fmt.Fprint(out, "\nWaiting for the load balancer address...")
	deadline := time.Now().Add(10 * time.Minute)
	for {
		addr, err := loadBalancerAddress(ctx, k)
		if err == nil && addr != "" {
			fmt.Fprintf(out, " %s\n", addr)
			return addr, nil
		}
		if time.Now().After(deadline) {
			return "", errors.New("the load balancer got no address in 10 minutes; check the cloud's load balancer quota and the ingress-nginx Service events")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
			fmt.Fprint(out, ".")
		}
	}
}

// waitForDNS tells the operator the record to create and waits until the
// platform hostname resolves to the load balancer, which Let's Encrypt
// needs before it can issue the first certificate.
func waitForDNS(ctx context.Context, cmd *cobra.Command, domain, addr string) error {
	out := cmd.OutOrStdout()
	host := "shpyrd." + domain
	if resolvesTo(host, addr) {
		fmt.Fprintf(out, "DNS: %s resolves to the load balancer.\n", host)
		return nil
	}
	fmt.Fprintf(out, "\nCreate this DNS record now (certificates are issued once it resolves):\n\n  *.%s   A   %s   (TTL 300)\n\nWaiting for %s to resolve to %s...", domain, addr, host, addr)
	deadline := time.Now().Add(30 * time.Minute)
	for {
		if resolvesTo(host, addr) {
			fmt.Fprintln(out, " resolved.")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not resolve to %s within 30 minutes; create the record and run `shpyrd cluster init` again", host, addr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
			fmt.Fprint(out, ".")
		}
	}
}

// resolvesTo asks public resolvers (what the certificate authority sees),
// not the operator's machine, whose cache may still hold the NXDOMAIN from
// the polls before the record existed.
func resolvesTo(host, addr string) bool {
	ips := publicLookup(host)
	if len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if ip == addr {
			return true
		}
	}
	// A hostname target (some clouds hand out names): compare resolutions.
	if net.ParseIP(addr) == nil {
		for _, w := range publicLookup(addr) {
			for _, ip := range ips {
				if ip == w {
					return true
				}
			}
		}
	}
	return false
}

// publicLookup resolves a name through well-known public resolvers.
func publicLookup(host string) []string {
	for _, server := range []string{"1.1.1.1:53", "8.8.8.8:53"} {
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, network, server)
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ips, err := r.LookupHost(ctx, host)
		cancel()
		if err == nil && len(ips) > 0 {
			return ips
		}
	}
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
	// Variables the operator set explicitly (--set, --registry-host) are
	// kept across runs; profile defaults stay live for everything else.
	for k, v := range info.Overrides {
		if k == install.VarRegistryHost && f.Changed("registry-host") {
			continue
		}
		if !hasSet(flags.set, k) {
			flags.set = append(flags.set, k+"="+v)
		}
	}
	return nil
}

// hasSet says --set already names the variable.
func hasSet(set []string, key string) bool {
	for _, kv := range set {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
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
		Short: "Delete the local kind cluster, or the platform on a cloud cluster",
		Long: `Without --context: deletes the local kind cluster and everything in it,
and undoes what was set up on this machine for it (Caddy site, local DNS).

With --context pointing at a cloud cluster: deletes what the platform
created in the cloud through Kubernetes, in order, waiting for the cloud to
confirm each step: every project (apps, databases, caches and volumes, with
their data), the load balancers, then the remaining disks (registry, server
data, monitoring). It ends with the command that removes the cluster and its
network, which belong to the infrastructure tooling (Terraform for Oracle
Cloud, see contrib/oci).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if g.kubeCtx != "" && !isKindContext(g.kubeCtx) {
				return destroyCloud(signalContext(), cmd, g, yes)
			}
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

func newClusterTrustCACmd(g *globalFlags) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:     "trust-ca",
		Aliases: []string{"trust"},
		Short:   "Install the platform CA in the operating system trust store",
		Long: `Installs the platform CA in the operating system trust store, so the
dashboard and applications of a local cluster, and the in-cluster registry
of any cluster, are trusted on this machine.

Local clusters share the development CA in ~/.shpyrd/ca. A cloud cluster
generated its own CA at install; with --context (or the current context)
pointing at it, that CA is fetched from the cluster and installed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			ctx := signalContext()
			if ca, name, ok := clusterCA(ctx, kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx}); ok {
				fmt.Fprintf(out, "Installing the platform CA of cluster %s (%s) into the system trust store (administrator rights required)...\n", name, ca.Cert.Subject.CommonName)
				return trustCA(out, ca)
			}
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
			return trustCA(out, ca)
		},
	}
	cmd.Flags().StringVar(&dir, "ca-dir", "", "directory of the development CA (default ~/.shpyrd/ca)")
	return cmd
}

func trustCA(out io.Writer, ca *localca.CA) error {
	res, err := ca.Trust()
	if err != nil {
		fmt.Fprintln(out, res.Instructions)
		return err
	}
	if res.Installed {
		fmt.Fprintln(out, "Platform CA trusted.")
	}
	if res.Instructions != "" {
		fmt.Fprintln(out, res.Instructions)
	}
	return nil
}

// clusterCA fetches the platform CA of a cluster whose CA was generated in
// the cluster (SHPYRD_CA_SOURCE=cluster) into ~/.shpyrd/clusters/<name>/,
// where Trust can read it. ok is false for local clusters and when the
// cluster cannot be reached.
func clusterCA(ctx context.Context, kopts kube.Options) (*localca.CA, string, bool) {
	k, err := kube.Connect(kopts)
	if err != nil {
		return nil, "", false
	}
	info, err := install.ReadInstallInfo(ctx, k, "")
	if err != nil || info == nil || info.Vars[install.VarCASource] != install.CASourceCluster {
		return nil, "", false
	}
	sec, err := k.Kube.CoreV1().Secrets("cert-manager").Get(ctx, install.LocalCASecretName, metav1.GetOptions{})
	if err != nil || len(sec.Data["tls.crt"]) == 0 {
		return nil, "", false
	}
	name := info.Vars[install.VarCluster]
	if name == "" {
		name = "cluster"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", false
	}
	dir := filepath.Join(home, ".shpyrd", "clusters", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", false
	}
	if err := os.WriteFile(filepath.Join(dir, "rootCA.pem"), sec.Data["tls.crt"], 0o644); err != nil {
		return nil, "", false
	}
	ca, err := localca.LoadCert(dir)
	if err != nil {
		return nil, "", false
	}
	return ca, name, true
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

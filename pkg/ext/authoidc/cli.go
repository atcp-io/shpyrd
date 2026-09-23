package authoidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"shpyrd/pkg/audit"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
)

// newAuthCmd returns `shpyrd auth` with the `oidc` subcommand. Other
// extensions add their own subcommands to the same `auth` command.
func newAuthCmd(g ext.CLIGlobals) *cobra.Command {
	auth := &cobra.Command{
		Use:   "auth",
		Short: "Configure how people sign in to the dashboard",
	}
	oidc := &cobra.Command{
		Use:   "oidc",
		Short: "Company identity providers: Okta or any OpenID Connect issuer (auth-oidc extension)",
	}
	oidc.AddCommand(newSetCmd(g), newListCmd(g), newRemoveCmd(g), newCheckCmd(g))
	auth.AddCommand(oidc)
	return auth
}

type cliDeps struct {
	k     *kube.Client
	store *Store
	info  *install.InstallInfo
}

func connect(g ext.CLIGlobals) (*cliDeps, error) {
	k, err := kube.Connect(kube.Options{Kubeconfig: g.Kubeconfig(), Context: g.Context()})
	if err != nil {
		return nil, err
	}
	d := &cliDeps{k: k, store: &Store{Kube: k.Kube, Namespace: install.DefaultSystemNamespace}}
	d.info, _ = install.ReadInstallInfo(context.Background(), k, install.DefaultSystemNamespace)
	return d, nil
}

func (d *cliDeps) dashboardURL() string {
	if d.info != nil {
		return strings.TrimRight(d.info.Vars[install.VarDashboardURL], "/")
	}
	return ""
}

func (d *cliDeps) enabled() bool {
	if d.info == nil {
		return false
	}
	for _, n := range strings.Split(d.info.Vars[install.VarExtensions], ",") {
		if strings.TrimSpace(n) == Name {
			return true
		}
	}
	return false
}

// restartServer rolls the server so it registers the providers anew.
func (d *cliDeps) restartServer(ctx context.Context) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"shpyrd.io/restarted-at":%q}}}}}`, time.Now().UTC().Format(time.RFC3339))
	_, err := d.k.Kube.AppsV1().Deployments(install.DefaultSystemNamespace).Patch(ctx, "shpyrd-server", types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("restart server: %w", err)
	}
	return nil
}

func (d *cliDeps) audit(ctx context.Context, action, target, detail string) {
	entry := audit.Entry{Actor: audit.LocalActor(), Action: action, Target: target, Detail: detail, Via: "cli"}
	_ = audit.Record(ctx, d.k.Kube, audit.ClusterRef(install.DefaultSystemNamespace), entry)
}

func cliContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx
}

func newSetCmd(g ext.CLIGlobals) *cobra.Command {
	var p Provider
	var scopes string
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Add or update an identity provider",
		Long: `Add or update an identity provider. Register the redirect URIs printed at the
end with the provider first; the client secret is stored in the cluster and
never printed again.

  shpyrd auth oidc set --id okta --label Okta --issuer https://acme.okta.com \
      --client-id 0oa... --client-secret "$OKTA_CLIENT_SECRET"

Read the secret from the environment or a file rather than typing it on the
command line, where the shell history keeps it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.HasPrefix(p.ClientSecret, "@") {
				raw, err := os.ReadFile(strings.TrimPrefix(p.ClientSecret, "@"))
				if err != nil {
					return err
				}
				p.ClientSecret = strings.TrimSpace(string(raw))
			}
			if scopes != "" {
				p.Scopes = strings.Fields(scopes)
			} else {
				p.Scopes = DefaultScopes
			}
			if p.Label == "" {
				p.Label = p.ID
			}
			if err := p.Validate(); err != nil {
				return err
			}
			ctx := cliContext()
			d, err := connect(g)
			if err != nil {
				return err
			}
			// Discovery from here catches typos before anything is stored.
			doc, err := discover(ctx, p.Issuer)
			if err != nil {
				return fmt.Errorf("issuer %s: %w", p.Issuer, err)
			}
			existed, err := d.store.Set(ctx, p)
			if err != nil {
				return err
			}
			verb := "Added"
			if existed {
				verb = "Updated"
			}
			d.audit(ctx, "auth.provider.set", p.ID, p.Issuer)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s provider %s (%s): %s\n", verb, p.ID, p.Label, p.Issuer)
			if doc.EndSessionEndpoint == "" {
				fmt.Fprintln(out, "Note: this issuer publishes no end_session_endpoint; signing out of shpyrd will not sign out of it.")
			}
			if base := d.dashboardURL(); base != "" {
				fmt.Fprintf(out, "Register at the provider:\n  Sign-in redirect URI:   %s/api/auth/callback\n  Sign-out redirect URI:  %s/\n", base, base)
			}
			if !d.enabled() {
				fmt.Fprintf(out, "The %s extension is not enabled on this cluster: run `shpyrd extensions enable %s`.\n", Name, Name)
				return nil
			}
			if err := d.restartServer(ctx); err != nil {
				return err
			}
			fmt.Fprintln(out, "The server is restarting; the button appears on the sign-in page in a few seconds.")
			return nil
		},
	}
	cmd.Flags().StringVar(&p.ID, "id", "", "provider id used in URLs and the audit trail (fixed once created), e.g. okta")
	cmd.Flags().StringVar(&p.Label, "label", "", "button text on the sign-in page (default: the id)")
	cmd.Flags().StringVar(&p.Issuer, "issuer", "", "issuer URL, e.g. https://acme.okta.com")
	cmd.Flags().StringVar(&p.ClientID, "client-id", "", "OAuth2 client id")
	cmd.Flags().StringVar(&p.ClientSecret, "client-secret", "", "OAuth2 client secret, or @path to read it from a file")
	cmd.Flags().StringVar(&scopes, "scopes", "", "extra scopes beyond openid email profile (default \"groups\"; pass \"-\" for none)")
	_ = cmd.MarkFlagRequired("id")
	_ = cmd.MarkFlagRequired("issuer")
	_ = cmd.MarkFlagRequired("client-id")
	_ = cmd.MarkFlagRequired("client-secret")
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if scopes == "-" {
			scopes = " " // Fields() -> none
		}
		return nil
	}
	return cmd
}

func newListCmd(g ext.CLIGlobals) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List identity providers",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			d, err := connect(g)
			if err != nil {
				return err
			}
			list, err := d.store.List(ctx)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No identity providers. Add one with `shpyrd auth oidc set`.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tLABEL\tISSUER\tCLIENT ID\tSCOPES")
			for _, p := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Label, p.Issuer, p.ClientID, strings.Join(append([]string{"openid", "email", "profile"}, p.Scopes...), " "))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if !d.enabled() {
				fmt.Fprintf(cmd.OutOrStdout(), "\nThe %s extension is not enabled: these providers are not offered on the sign-in page.\n", Name)
			}
			return nil
		},
	}
}

func newRemoveCmd(g ext.CLIGlobals) *cobra.Command {
	return &cobra.Command{
		Use:     "remove <id>",
		Short:   "Remove an identity provider (roles are kept: they are keyed by email)",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			d, err := connect(g)
			if err != nil {
				return err
			}
			if err := d.store.Remove(ctx, args[0]); err != nil {
				return err
			}
			d.audit(ctx, "auth.provider.remove", args[0], "")
			fmt.Fprintf(cmd.OutOrStdout(), "Removed provider %s\n", args[0])
			if d.enabled() {
				if err := d.restartServer(ctx); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newCheckCmd(g ext.CLIGlobals) *cobra.Command {
	return &cobra.Command{
		Use:   "check <id|issuer-url>",
		Short: "Fetch an issuer's discovery document and report what it supports",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			issuer := args[0]
			var scopes []string
			if !strings.HasPrefix(issuer, "https://") {
				d, err := connect(g)
				if err != nil {
					return err
				}
				p, err := d.store.Get(ctx, issuer)
				if err != nil {
					return fmt.Errorf("no provider %q; give its id or an https issuer URL", issuer)
				}
				issuer, scopes = p.Issuer, p.Scopes
			}
			doc, err := discover(ctx, issuer)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Issuer:         %s\n", doc.Issuer)
			fmt.Fprintf(out, "Authorization:  %s\n", doc.AuthorizationEndpoint)
			fmt.Fprintf(out, "Token:          %s\n", doc.TokenEndpoint)
			fmt.Fprintf(out, "Sign-out:       %s\n", firstNonEmpty(doc.EndSessionEndpoint, "not supported (RP-initiated logout unavailable)"))
			fmt.Fprintf(out, "PKCE:           %s\n", supports(doc.CodeChallengeMethods, "S256", "S256 supported", "S256 not advertised"))
			fmt.Fprintf(out, "Scopes:         %s\n", strings.Join(doc.ScopesSupported, " "))
			fmt.Fprintf(out, "Claims:         %s\n", strings.Join(doc.ClaimsSupported, " "))
			switch {
			case contains(doc.ClaimsSupported, "groups") || contains(doc.ScopesSupported, "groups"):
				fmt.Fprintln(out, "Groups:         advertised; make sure the groups claim is configured on the application (Okta: Sign On > OpenID Connect ID Token > Groups claim filter).")
			case len(doc.ClaimsSupported) == 0 && len(doc.ScopesSupported) == 0:
				fmt.Fprintln(out, "Groups:         the issuer does not list scopes or claims; groups may still arrive if configured there.")
			default:
				fmt.Fprintln(out, "Groups:         not advertised; users will sign in without groups unless the issuer is configured to send a `groups` claim.")
			}
			if len(scopes) > 0 {
				fmt.Fprintf(out, "Requested:      openid email profile %s\n", strings.Join(scopes, " "))
			}
			return nil
		},
	}
}

// discoveryDoc is the part of the OpenID configuration the CLI reports.
type discoveryDoc struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	EndSessionEndpoint    string   `json:"end_session_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	ClaimsSupported       []string `json:"claims_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

func discover(ctx context.Context, issuer string) (*discoveryDoc, error) {
	u := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery: %s returned %s", u, resp.Status)
	}
	var doc discoveryDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if doc.Issuer == "" || doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return nil, errors.New("discovery: document lacks issuer, authorization or token endpoint")
	}
	if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return nil, fmt.Errorf("discovery: document says the issuer is %s; use that URL", doc.Issuer)
	}
	return &doc, nil
}

func supports(list []string, want, yes, no string) string {
	if contains(list, want) {
		return yes
	}
	return no
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

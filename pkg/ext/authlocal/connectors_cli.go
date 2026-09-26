package authlocal

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// newAuthConnectorCmd returns `shpyrd auth` with the `connector`
// subcommand; the CLI merges it with the `auth` command of auth-oidc.
func newAuthConnectorCmd(g ext.CLIGlobals) *cobra.Command {
	auth := &cobra.Command{
		Use:   "auth",
		Short: "Configure how people sign in to the dashboard",
	}
	conn := &cobra.Command{
		Use:   "connector",
		Short: "GitHub and Google sign-in through the bundled issuer (auth-local extension)",
		Long: `GitHub and Google sign in through Dex, the issuer the auth-local extension
installs. Register an OAuth application at the provider with the callback URL
printed by this command, then:

  shpyrd auth connector add github --client-id ... --client-secret "$GITHUB_CLIENT_SECRET" --org acme
  shpyrd auth connector add google --client-id ... --client-secret "$GOOGLE_CLIENT_SECRET" --hosted-domain acme.com
  shpyrd auth connector list
  shpyrd auth connector remove github

GitHub teams arrive as groups named org:team (for Teams); Google groups need
a service account and are not configured by this command.`,
	}
	conn.AddCommand(newConnectorAddCmd(g), newConnectorListCmd(g), newConnectorRemoveCmd(g))
	auth.AddCommand(conn)
	return auth
}

type connectorDeps struct {
	k     *kube.Client
	store *ConnectorStore
	info  *install.InstallInfo
}

func connectorDepsFor(g ext.CLIGlobals) (*connectorDeps, error) {
	k, err := kube.Connect(kube.Options{Kubeconfig: g.Kubeconfig(), Context: g.Context()})
	if err != nil {
		return nil, err
	}
	d := &connectorDeps{k: k, store: &ConnectorStore{Dynamic: k.Dynamic, Namespace: install.DefaultSystemNamespace}}
	if d.info, _ = install.ReadInstallInfo(context.Background(), k, install.DefaultSystemNamespace); d.info != nil {
		d.store.Issuer = d.info.Vars[install.VarAuthURL]
	}
	return d, nil
}

func (d *connectorDeps) restartServer(ctx context.Context) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"shpyrd.io/restarted-at":%q}}}}}`, time.Now().UTC().Format(time.RFC3339))
	if _, err := d.k.Kube.AppsV1().Deployments(install.DefaultSystemNamespace).Patch(ctx, "shpyrd-server", types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("restart server: %w", err)
	}
	return nil
}

func (d *connectorDeps) audit(ctx context.Context, action, target, detail string) {
	entry := audit.Entry{Actor: audit.LocalActor(), Action: action, Target: target, Detail: detail, Via: "cli"}
	_ = audit.Record(ctx, d.k.Kube, audit.ClusterRef(install.DefaultSystemNamespace), entry)
}

func newConnectorAddCmd(g ext.CLIGlobals) *cobra.Command {
	var spec ConnectorSpec
	cmd := &cobra.Command{
		Use:   "add <github|google|microsoft|oidc>",
		Short: "Add or replace a connector",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec.Type = args[0]
			if strings.HasPrefix(spec.ClientSecret, "@") {
				raw, err := os.ReadFile(strings.TrimPrefix(spec.ClientSecret, "@"))
				if err != nil {
					return err
				}
				spec.ClientSecret = strings.TrimSpace(string(raw))
			}
			if err := spec.Validate(); err != nil {
				return err
			}
			ctx := cliContext()
			d, err := connectorDepsFor(g)
			if err != nil {
				return err
			}
			existed, err := d.store.Add(ctx, spec)
			if err != nil {
				return err
			}
			verb := "Added"
			if existed {
				verb = "Updated"
			}
			d.audit(ctx, "auth.connector.set", spec.ID, spec.Type)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s connector %s (%s, %s)\n", verb, spec.ID, spec.Name, spec.Type)
			fmt.Fprintf(out, "Callback URL to register at %s: %s/callback\n", spec.Name, strings.TrimRight(d.store.Issuer, "/"))
			if err := d.restartServer(ctx); err != nil {
				return err
			}
			fmt.Fprintln(out, "The server is restarting; the button appears on the sign-in page in a few seconds.")
			return nil
		},
	}
	cmd.Flags().StringVar(&spec.ID, "id", "", "connector id (default: the type)")
	cmd.Flags().StringVar(&spec.Name, "label", "", "button text on the sign-in page (default: the provider's name)")
	cmd.Flags().StringVar(&spec.ClientID, "client-id", "", "OAuth application client id")
	cmd.Flags().StringVar(&spec.ClientSecret, "client-secret", "", "OAuth application client secret, or @path to read it from a file")
	cmd.Flags().StringVar(&spec.Org, "org", "", "GitHub: only members of this organisation may sign in; its teams become groups")
	cmd.Flags().StringVar(&spec.HostedDomain, "hosted-domain", "", "Google: only accounts of this Workspace domain may sign in")
	cmd.Flags().StringVar(&spec.Tenant, "tenant", "", "Microsoft: only accounts of this Entra tenant (id or domain) may sign in")
	cmd.Flags().StringVar(&spec.Issuer, "issuer", "", "oidc: the provider's issuer URL (Okta, Keycloak, Auth0, ...)")
	_ = cmd.MarkFlagRequired("client-id")
	_ = cmd.MarkFlagRequired("client-secret")
	return cmd
}

func newConnectorListCmd(g ext.CLIGlobals) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List connectors",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			d, err := connectorDepsFor(g)
			if err != nil {
				return err
			}
			list, err := d.store.List(ctx)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No connectors. Add one with `shpyrd auth connector add github|google`.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTYPE\tLABEL\tDETAIL")
			for _, c := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.ID, c.Type, c.Name, c.Detail)
			}
			return tw.Flush()
		},
	}
}

func newConnectorRemoveCmd(g ext.CLIGlobals) *cobra.Command {
	return &cobra.Command{
		Use:     "remove <id>",
		Short:   "Remove a connector",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			d, err := connectorDepsFor(g)
			if err != nil {
				return err
			}
			if err := d.store.Remove(ctx, args[0]); err != nil {
				return err
			}
			d.audit(ctx, "auth.connector.remove", args[0], "")
			fmt.Fprintf(cmd.OutOrStdout(), "Removed connector %s\n", args[0])
			return d.restartServer(ctx)
		},
	}
}

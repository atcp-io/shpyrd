package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/ext/resources"
	"shpyrd/pkg/kexec"
)

func newPgCmd(g ext.CLIGlobals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pg",
		Aliases: []string{"postgres"},
		Short:   "PostgreSQL databases of a project (extension postgres)",
		Long: `Create a PostgreSQL database in a project and attach it to the app:

  shpyrd pg create db --project shop --size shared-m --storage 10Gi
  shpyrd attach db --project shop        # DATABASE_URL, DATABASE_HOST, ... in the app
  shpyrd pg psql db --project shop       # a psql session on the primary

Databases run on CloudNativePG; one cluster per database, 1 instance by
default (2-3 for high availability with --instances).`,
	}
	cmd.AddCommand(newCreateCmd(g), newListCmd(g), newInfoCmd(g), newPsqlCmd(g), newDeleteCmd(g))
	return cmd
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

func projectFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "project", "", "project name (required)")
	_ = cmd.MarkFlagRequired("project")
}

func newCreateCmd(g ext.CLIGlobals) *cobra.Command {
	var (
		project   string
		version   string
		size      string
		storage   string
		instances int32
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			if err := resources.RequireProject(ctx, c, project); err != nil {
				return err
			}
			qty, err := resource.ParseQuantity(storage)
			if err != nil || qty.Sign() <= 0 {
				return fmt.Errorf("invalid --storage %q (use e.g. 10Gi)", storage)
			}
			pg := &shpyrdv1.Postgres{
				ObjectMeta: metav1.ObjectMeta{Name: args[0], Namespace: resources.Namespace(project), Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", shpyrdv1.LabelProject: project}},
				Spec:       shpyrdv1.PostgresSpec{Version: version, Size: size, Storage: &qty, Instances: ptr.To(instances)},
			}
			if err := c.Create(ctx, pg); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("database %q already exists in project %s", args[0], project)
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Creating PostgreSQL %s database %s (%s, %d instance(s))...\n", version, args[0], qty.String(), instances)
			return waitReady(ctx, cmd, c, pg, 5*time.Minute)
		},
	}
	projectFlag(cmd, &project)
	cmd.Flags().StringVar(&version, "version", "17", "PostgreSQL major version")
	cmd.Flags().StringVar(&size, "size", "", "instance size from the catalog (default: the catalog default)")
	cmd.Flags().StringVar(&storage, "storage", "5Gi", "data volume size")
	cmd.Flags().Int32Var(&instances, "instances", 1, "number of instances (2-3 for high availability)")
	return cmd
}

// waitReady follows the resource until it is Ready or Failed.
func waitReady(ctx context.Context, cmd *cobra.Command, c client.Client, pg *shpyrdv1.Postgres, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		cur := &shpyrdv1.Postgres{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(pg), cur); err != nil {
			return err
		}
		if msg := cur.Status.Phase + " " + cur.Status.Message; msg != last && cur.Status.Phase != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "    %s\n", strings.TrimSpace(msg))
			last = msg
		}
		switch cur.Status.Phase {
		case shpyrdv1.ResourceReady:
			fmt.Fprintf(cmd.OutOrStdout(), "Database %s is ready at %s. Attach it with `shpyrd attach %s --project %s`.\n", pg.Name, cur.Status.Endpoint, pg.Name, strings.TrimPrefix(pg.Namespace, "app-"))
			return nil
		case shpyrdv1.ResourceFailed:
			if strings.Contains(cur.Status.Message, "extension is not installed") {
				return errors.New(resources.ExtensionHint(Name))
			}
			return errors.New(cur.Status.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Still provisioning; check with `shpyrd pg list`.")
	return nil
}

func newListCmd(g ext.CLIGlobals) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the databases of a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			var list shpyrdv1.PostgresList
			if err := c.List(ctx, &list, client.InNamespace(resources.Namespace(project))); err != nil {
				return err
			}
			if len(list.Items) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No databases in project %s. Create one with `shpyrd pg create db --project %s`.\n", project, project)
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tVERSION\tSIZE\tSTORAGE\tINSTANCES\tSTATUS\tATTACHED TO")
			for _, pg := range list.Items {
				bound, _ := resources.BoundBy(ctx, c, pg.Namespace, "Postgres", pg.Name)
				storage := "5Gi"
				if pg.Spec.Storage != nil {
					storage = pg.Spec.Storage.String()
				}
				if pg.Status.Storage != "" && pg.Status.Storage != storage {
					storage = pg.Status.Storage + " (" + storage + " requested)"
				}
				inst := int32(1)
				if pg.Spec.Instances != nil {
					inst = *pg.Spec.Instances
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", pg.Name, firstNonEmpty(pg.Spec.Version, "17"), firstNonEmpty(pg.Spec.Size, "default"), storage, inst, firstNonEmpty(pg.Status.Phase, "Pending"), firstNonEmpty(strings.Join(bound, ", "), "-"))
			}
			return tw.Flush()
		},
	}
	projectFlag(cmd, &project)
	return cmd
}

func newInfoCmd(g ext.CLIGlobals) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "info <name>",
		Short: "Show a database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			pg, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			bound, _ := resources.BoundBy(ctx, c, pg.Namespace, "Postgres", pg.Name)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Database:   %s (project %s)\n", pg.Name, project)
			fmt.Fprintf(out, "Status:     %s%s\n", firstNonEmpty(pg.Status.Phase, "Pending"), suffix(pg.Status.Message))
			fmt.Fprintf(out, "Endpoint:   %s\n", firstNonEmpty(pg.Status.Endpoint, "-"))
			fmt.Fprintf(out, "Version:    PostgreSQL %s\n", firstNonEmpty(pg.Spec.Version, "17"))
			fmt.Fprintf(out, "Attached:   %s\n", firstNonEmpty(strings.Join(bound, ", "), "- (shpyrd attach "+pg.Name+" --project "+project+")"))
			fmt.Fprintf(out, "Config vars: DATABASE_URL, DATABASE_HOST, DATABASE_PORT, DATABASE_USER, DATABASE_PASSWORD, DATABASE_NAME (values are never shown)\n")
			return nil
		},
	}
	projectFlag(cmd, &project)
	return cmd
}

func newPsqlCmd(g ext.CLIGlobals) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "psql <name> [-- psql args...]",
		Short: "Open psql on the database's primary instance",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			k, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			pg, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			var pods corev1.PodList
			if err := c.List(ctx, &pods, client.InNamespace(pg.Namespace), client.MatchingLabels{"cnpg.io/cluster": pg.Name, "cnpg.io/instanceRole": "primary"}); err != nil {
				return err
			}
			if len(pods.Items) == 0 {
				return fmt.Errorf("database %s has no primary instance yet (%s)", pg.Name, firstNonEmpty(pg.Status.Message, pg.Status.Phase))
			}
			command := append([]string{"psql", "-d", "app"}, args[1:]...)
			fmt.Fprintf(cmd.ErrOrStderr(), "Connecting to %s (primary)...\n", pg.Name)
			return kexec.RemoteExit(kexec.Exec(ctx, k, pg.Namespace, pods.Items[0].Name, "postgres", command, kexec.StdinIsTerminal()))
		},
	}
	projectFlag(cmd, &project)
	return cmd
}

func newDeleteCmd(g ext.CLIGlobals) *cobra.Command {
	var (
		project string
		yes     bool
		force   bool
	)
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm", "destroy"},
		Short:   "Delete a database and its data",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			pg, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			if err := resources.CheckDeletable(ctx, c, pg.Namespace, "Postgres", pg.Name, force); err != nil {
				return err
			}
			if !yes {
				return fmt.Errorf("this deletes database %q and all its data; re-run with --yes to confirm", pg.Name)
			}
			if err := c.Delete(ctx, pg); client.IgnoreNotFound(err) != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted database %s from project %s\n", pg.Name, project)
			return nil
		},
	}
	projectFlag(cmd, &project)
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm")
	cmd.Flags().BoolVar(&force, "force", false, "delete even while attached to an app")
	return cmd
}

func get(ctx context.Context, c client.Client, project, name string) (*shpyrdv1.Postgres, error) {
	pg := &shpyrdv1.Postgres{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: resources.Namespace(project), Name: name}, pg); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("database %q not found in project %s (see `shpyrd pg list --project %s`)", name, project, project)
		}
		return nil, err
	}
	return pg, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func suffix(msg string) string {
	if msg == "" {
		return ""
	}
	return " (" + msg + ")"
}

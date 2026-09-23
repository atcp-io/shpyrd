package redis

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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/ext/resources"
	"shpyrd/pkg/kexec"
)

func newRedisCmd(g ext.CLIGlobals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "redis",
		Short: "Redis-compatible caches and queues of a project (extension redis)",
		Long: `Create a Redis-compatible store (Valkey by default) in a project and attach
it to the app:

  shpyrd redis create cache --project shop                  # cache: LRU eviction, data lost on restart
  shpyrd redis create queue --project shop --persistent     # queue: append-only file on a volume
  shpyrd attach cache --project shop                        # REDIS_URL, REDIS_HOST, ... in the app
  shpyrd redis cli cache --project shop`,
	}
	cmd.AddCommand(newCreateCmd(g), newListCmd(g), newInfoCmd(g), newCliCmd(g), newDeleteCmd(g))
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
		project    string
		engine     string
		version    string
		size       string
		persistent bool
		storage    string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a store",
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
			rd := &shpyrdv1.Redis{
				ObjectMeta: metav1.ObjectMeta{Name: args[0], Namespace: resources.Namespace(project), Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", shpyrdv1.LabelProject: project}},
				Spec:       shpyrdv1.RedisSpec{Engine: engine, Version: version, Size: size, Persistent: persistent},
			}
			if persistent {
				qty, err := resource.ParseQuantity(storage)
				if err != nil || qty.Sign() <= 0 {
					return fmt.Errorf("invalid --storage %q (use e.g. 1Gi)", storage)
				}
				rd.Spec.Storage = &qty
			}
			if err := c.Create(ctx, rd); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("store %q already exists in project %s", args[0], project)
				}
				return err
			}
			mode := "cache"
			if persistent {
				mode = "persistent"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Creating %s %s (%s)...\n", firstNonEmpty(engine, "valkey"), args[0], mode)
			return waitReady(ctx, cmd, c, rd, 3*time.Minute)
		},
	}
	projectFlag(cmd, &project)
	cmd.Flags().StringVar(&engine, "engine", "valkey", "valkey or redis")
	cmd.Flags().StringVar(&version, "version", "", "engine major version (default: valkey 8, redis 7)")
	cmd.Flags().StringVar(&size, "size", "", "instance size from the catalog (default: the catalog default)")
	cmd.Flags().BoolVar(&persistent, "persistent", false, "keep data on a volume (append-only file); default is a cache that loses data on restart")
	cmd.Flags().StringVar(&storage, "storage", "1Gi", "volume size when --persistent")
	return cmd
}

func waitReady(ctx context.Context, cmd *cobra.Command, c client.Client, rd *shpyrdv1.Redis, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		cur := &shpyrdv1.Redis{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(rd), cur); err != nil {
			return err
		}
		if msg := cur.Status.Phase + " " + cur.Status.Message; msg != last && cur.Status.Phase != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "    %s\n", strings.TrimSpace(msg))
			last = msg
		}
		switch cur.Status.Phase {
		case shpyrdv1.ResourceReady:
			fmt.Fprintf(cmd.OutOrStdout(), "%s is ready at %s. Attach it with `shpyrd attach %s --project %s`.\n", rd.Name, cur.Status.Endpoint, rd.Name, strings.TrimPrefix(rd.Namespace, "app-"))
			return nil
		case shpyrdv1.ResourceFailed:
			return errors.New(cur.Status.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	// No status at all means no controller: the extension is off.
	cur := &shpyrdv1.Redis{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(rd), cur); err == nil && cur.Status.Phase == "" {
		return errors.New(resources.ExtensionHint(Name))
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Still starting; check with `shpyrd redis list`.")
	return nil
}

func newListCmd(g ext.CLIGlobals) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the stores of a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			var list shpyrdv1.RedisList
			if err := c.List(ctx, &list, client.InNamespace(resources.Namespace(project))); err != nil {
				return err
			}
			if len(list.Items) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No stores in project %s. Create one with `shpyrd redis create cache --project %s`.\n", project, project)
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tENGINE\tSIZE\tMODE\tSTATUS\tATTACHED TO")
			for _, rd := range list.Items {
				bound, _ := resources.BoundBy(ctx, c, rd.Namespace, "Redis", rd.Name)
				mode := "cache"
				if rd.Spec.Persistent {
					mode = "persistent"
					if rd.Spec.Storage != nil {
						mode += " " + rd.Spec.Storage.String()
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", rd.Name, firstNonEmpty(rd.Spec.Engine, "valkey"), firstNonEmpty(rd.Spec.Size, "default"), mode, firstNonEmpty(rd.Status.Phase, "Pending"), firstNonEmpty(strings.Join(bound, ", "), "-"))
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
		Short: "Show a store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			rd, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			bound, _ := resources.BoundBy(ctx, c, rd.Namespace, "Redis", rd.Name)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Store:      %s (project %s)\n", rd.Name, project)
			fmt.Fprintf(out, "Status:     %s%s\n", firstNonEmpty(rd.Status.Phase, "Pending"), suffix(rd.Status.Message))
			fmt.Fprintf(out, "Endpoint:   %s\n", firstNonEmpty(rd.Status.Endpoint, "-"))
			fmt.Fprintf(out, "Attached:   %s\n", firstNonEmpty(strings.Join(bound, ", "), "- (shpyrd attach "+rd.Name+" --project "+project+")"))
			fmt.Fprintf(out, "Config vars: REDIS_URL, REDIS_HOST, REDIS_PORT, REDIS_PASSWORD (values are never shown)\n")
			return nil
		},
	}
	projectFlag(cmd, &project)
	return cmd
}

func newCliCmd(g ext.CLIGlobals) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "cli <name> [-- args...]",
		Short: "Open the engine's CLI (valkey-cli or redis-cli) on the store",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			k, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			rd, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			engine := firstNonEmpty(rd.Spec.Engine, "valkey")
			extra := ""
			if len(args) > 1 {
				extra = " " + shellJoin(args[1:])
			}
			command := []string{"sh", "-c", fmt.Sprintf(`exec %s-cli -a "$REDIS_PASSWORD" --no-auth-warning%s`, engine, extra)}
			fmt.Fprintf(cmd.ErrOrStderr(), "Connecting to %s...\n", rd.Name)
			return kexec.RemoteExit(kexec.Exec(ctx, k, rd.Namespace, rd.Name+"-0", "redis", command, kexec.StdinIsTerminal()))
		},
	}
	projectFlag(cmd, &project)
	return cmd
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
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
		Short:   "Delete a store (and its data when persistent)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			rd, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			if err := resources.CheckDeletable(ctx, c, rd.Namespace, "Redis", rd.Name, force); err != nil {
				return err
			}
			if !yes {
				return fmt.Errorf("this deletes store %q; re-run with --yes to confirm", rd.Name)
			}
			if err := c.Delete(ctx, rd); client.IgnoreNotFound(err) != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s from project %s\n", rd.Name, project)
			return nil
		},
	}
	projectFlag(cmd, &project)
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm")
	cmd.Flags().BoolVar(&force, "force", false, "delete even while attached to an app")
	return cmd
}

func get(ctx context.Context, c client.Client, project, name string) (*shpyrdv1.Redis, error) {
	rd := &shpyrdv1.Redis{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: resources.Namespace(project), Name: name}, rd); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("store %q not found in project %s (see `shpyrd redis list --project %s`)", name, project, project)
		}
		return nil, err
	}
	return rd, nil
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

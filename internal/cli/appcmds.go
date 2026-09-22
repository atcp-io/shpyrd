package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/configvars"
)

// appFlag adds the shared --project flag (--app kept as a hidden alias).
func appFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "project", "", "project name (default from shpyrd.yaml)")
	cmd.Flags().StringVarP(dst, "app", "a", "", "alias of --project")
	_ = cmd.Flags().MarkHidden("app")
}

// ---- secrets ---------------------------------------------------------------

func newSecretsCmd(g *globalFlags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage the project's config vars",
		Long: `Config vars are injected into every process as environment variables.
Changing them creates a new release and restarts the processes. Values are
write-only: they are never printed back.`,
	}
	set := &cobra.Command{
		Use:   "set KEY=VALUE [KEY=VALUE...]",
		Short: "Set config vars",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set := map[string]string{}
			for _, kv := range args {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return fmt.Errorf("expected KEY=VALUE, got %q", kv)
				}
				set[k] = v
			}
			return mutateEnvSecret(g, cmd, appName, set, nil)
		},
	}
	unset := &cobra.Command{
		Use:   "unset KEY [KEY...]",
		Short: "Remove config vars",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return mutateEnvSecret(g, cmd, appName, nil, args)
		},
	}
	list := &cobra.Command{
		Use:     "list",
		Short:   "List config var names (values are never shown)",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if _, err := ac.getApp(ctx, name); err != nil {
				return err
			}
			sec := &corev1.Secret{}
			if err := ac.c.Get(ctx, types.NamespacedName{Namespace: appNamespace(name), Name: name + shpyrdv1.EnvSecretSuffix}, sec); err != nil {
				if apierrors.IsNotFound(err) {
					fmt.Fprintln(cmd.OutOrStdout(), "no config vars set")
					return nil
				}
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tUPDATED")
			for _, v := range configvars.List(sec) {
				when := "-"
				if t, err := time.Parse(time.RFC3339, v.UpdatedAt); err == nil {
					when = age(metav1.NewTime(t))
				}
				fmt.Fprintf(tw, "%s\t%s\n", v.Name, when)
			}
			return tw.Flush()
		},
	}
	for _, c := range []*cobra.Command{set, unset, list} {
		appFlag(c, &appName)
	}
	cmd.AddCommand(set, unset, list)
	return cmd
}

func mutateEnvSecret(g *globalFlags, cmd *cobra.Command, appName string, set map[string]string, unset []string) error {
	ctx := signalContext()
	name, err := resolveAppName(appName)
	if err != nil {
		return err
	}
	ac, err := newAppClient(g, cmd.OutOrStdout())
	if err != nil {
		return err
	}
	app, err := ac.getApp(ctx, name)
	if err != nil {
		return err
	}
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.EnvSecretName()}
	sec := &corev1.Secret{}
	create := false
	if err := ac.c.Get(ctx, key, sec); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		create = true
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{shpyrdv1.LabelApp: name}},
			Type:       corev1.SecretTypeOpaque,
		}
	}
	if err := configvars.Apply(sec, set, unset, time.Now()); err != nil {
		return err
	}
	if create {
		err = ac.c.Create(ctx, sec)
	} else {
		err = ac.c.Update(ctx, sec)
	}
	if err != nil {
		return err
	}
	names := make([]string, 0, len(sec.Data))
	for _, v := range configvars.List(sec) {
		names = append(names, v.Name)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Config vars for %s: %s\n", name, strings.Join(names, ", "))
	if app.Status.Image != "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Restarting processes with the new configuration...")
	}
	return nil
}

// ---- scale -----------------------------------------------------------------

func newScaleCmd(g *globalFlags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "scale PROCESS=N [PROCESS=N...]",
		Short: "Set the number of replicas per process type, e.g. web=2 worker=1",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			changes := map[string]int32{}
			for _, kv := range args {
				proc, n, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("expected PROCESS=N, got %q", kv)
				}
				v, err := strconv.Atoi(n)
				if err != nil || v < 0 {
					return fmt.Errorf("invalid replica count %q", n)
				}
				changes[proc] = int32(v)
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			app, err := ac.updateApp(ctx, name, func(a *shpyrdv1.App) error {
				if a.Spec.Processes == nil {
					a.Spec.Processes = map[string]shpyrdv1.Process{"web": {}}
				}
				for proc, n := range changes {
					p := a.Spec.Processes[proc]
					p.Replicas = ptr.To(n)
					a.Spec.Processes[proc] = p
				}
				return nil
			})
			if err != nil {
				return err
			}
			names := make([]string, 0, len(app.Spec.Processes))
			for n := range app.Spec.Processes {
				names = append(names, n)
			}
			sort.Strings(names)
			var parts []string
			for _, n := range names {
				r := int32(1)
				if app.Spec.Processes[n].Replicas != nil {
					r = *app.Spec.Processes[n].Replicas
				}
				parts = append(parts, fmt.Sprintf("%s=%d", n, r))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Scaling %s: %s\n", name, strings.Join(parts, " "))
			return nil
		},
	}
	appFlag(cmd, &appName)
	return cmd
}

// ---- logs ------------------------------------------------------------------

func newLogsCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		process string
		follow  bool
		tail    int64
		build   bool
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print the project's logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			app, err := ac.getApp(ctx, name)
			if err != nil {
				return err
			}
			if build {
				if app.Status.LatestBuild == "" {
					return errors.New("no build yet")
				}
				return ac.followBuild(ctx, app.Namespace, app.Status.LatestBuild)
			}
			// Build pods inherit the app label from the kpack Image; only
			// workloads carry the process label.
			selector := shpyrdv1.LabelApp + "=" + name + "," + shpyrdv1.LabelProcess
			if process != "" {
				selector = shpyrdv1.LabelApp + "=" + name + "," + shpyrdv1.LabelProcess + "=" + process
			}
			return ac.streamPodLogs(ctx, app.Namespace, selector, follow, tail)
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().StringVarP(&process, "process", "p", "", "only this process type (web, worker...)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new log lines")
	cmd.Flags().Int64VarP(&tail, "tail", "n", 100, "number of recent lines per pod (-1 for all)")
	cmd.Flags().BoolVar(&build, "build", false, "show the latest build's logs instead")
	return cmd
}

// ---- releases / rollback -----------------------------------------------------

func newReleasesCmd(g *globalFlags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "releases",
		Short: "List the release history",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			app, err := ac.getApp(ctx, name)
			if err != nil {
				return err
			}
			if len(app.Status.Releases) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no releases yet")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "RELEASE\tCREATED\tDIGEST\tDESCRIPTION")
			for i := len(app.Status.Releases) - 1; i >= 0; i-- {
				r := app.Status.Releases[i]
				cur := ""
				if i == len(app.Status.Releases)-1 {
					cur = " (current)"
				}
				fmt.Fprintf(tw, "v%d%s\t%s\t%s\t%s\n", r.Number, cur, r.CreatedAt.Format(time.DateTime), digest(r.Image), r.Description)
			}
			return tw.Flush()
		},
	}
	appFlag(cmd, &appName)
	return cmd
}

func newRollbackCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		noWait  bool
		force   bool
	)
	cmd := &cobra.Command{
		Use:   "rollback [RELEASE]",
		Short: "Roll back to a previous release (default: the one before the current)",
		Long: `Roll back re-releases an earlier release: its build is pinned and its
config vars are restored. The source configuration is kept; the next
'shpyrd deploy' unpins the build.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			app, err := ac.getApp(ctx, name)
			if err != nil {
				return err
			}
			var target *shpyrdv1.Release
			if len(args) == 1 {
				n, err := strconv.Atoi(strings.TrimPrefix(args[0], "v"))
				if err != nil {
					return fmt.Errorf("invalid release %q", args[0])
				}
				target = app.ReleaseByNumber(n)
				if target == nil {
					return fmt.Errorf("release v%d not found", n)
				}
			} else {
				if len(app.Status.Releases) < 2 {
					return errors.New("no previous release to roll back to")
				}
				target = &app.Status.Releases[len(app.Status.Releases)-2]
			}
			if cur := app.CurrentRelease(); cur != nil && cur.Number == target.Number {
				return fmt.Errorf("v%d is the current release", target.Number)
			}
			if !force && (app.Status.Phase == shpyrdv1.PhaseDeploying || app.Status.Phase == shpyrdv1.PhaseBuilding || app.Status.ObservedGeneration < app.Generation) {
				return fmt.Errorf("a release is still rolling out (%s); wait for it or pass --force", firstNonEmpty(app.Status.Message, app.Status.Phase))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "==> Rolling back %s to v%d (build %s, config as of v%d)\n", name, target.Number, digest(target.Image), target.Number)
			img := target.Image
			note := fmt.Sprintf("Rollback to v%d", target.Number)
			sizesOf := target.Sizes
			updated, err := ac.updateApp(ctx, name, func(a *shpyrdv1.App) error {
				a.Spec.Image = img
				for proc, size := range sizesOf {
					if p, ok := a.Spec.Processes[proc]; ok && size != "custom" {
						p.Size = size
						p.Resources = corev1.ResourceRequirements{}
						a.Spec.Processes[proc] = p
					}
				}
				if a.Annotations == nil {
					a.Annotations = map[string]string{}
				}
				a.Annotations[shpyrdv1.AnnotationReleaseNote] = note
				a.Annotations[shpyrdv1.AnnotationRollbackTo] = fmt.Sprint(target.Number)
				return nil
			})
			if err != nil {
				return err
			}
			if noWait {
				return nil
			}
			final, err := ac.waitRunning(ctx, name, updated.Generation, 10*time.Minute)
			if err != nil {
				return err
			}
			if rel := final.CurrentRelease(); rel != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "\nReleased v%d: %s\n", rel.Number, rel.Description)
			}
			return nil
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return immediately")
	cmd.Flags().BoolVar(&force, "force", false, "roll back even while another release is rolling out")
	return cmd
}

// ---- open ------------------------------------------------------------------

func newOpenCmd(g *globalFlags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "open",
		Short: "Open the project in the browser",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			app, err := ac.getApp(ctx, name)
			if err != nil {
				return err
			}
			if app.Status.URL == "" {
				return errors.New("the project has no URL yet (no web process deployed)")
			}
			fmt.Fprintln(cmd.OutOrStdout(), app.Status.URL)
			return openBrowser(app.Status.URL)
		},
	}
	appFlag(cmd, &appName)
	return cmd
}

func openBrowser(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	return c.Start()
}

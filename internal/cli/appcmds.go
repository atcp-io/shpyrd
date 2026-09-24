package cli

import (
	"encoding/json"
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
	"k8s.io/apimachinery/pkg/api/meta"
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
				if !apierrors.IsNotFound(err) {
					return err
				}
				sec = nil
			}
			// Variables provided by attached resources (read-only).
			bound := &corev1.Secret{}
			if err := ac.c.Get(ctx, types.NamespacedName{Namespace: appNamespace(name), Name: name + shpyrdv1.BindingsSecretSuffix}, bound); err != nil {
				bound = nil
			}
			// Global vars the project receives (RFC-0016), from the mirror
			// the controller keeps in the project namespace.
			global := &corev1.Secret{}
			if err := ac.c.Get(ctx, types.NamespacedName{Namespace: appNamespace(name), Name: shpyrdv1.GlobalEnvSecretName}, global); err != nil {
				global = nil
			}
			if sec == nil && bound == nil && global == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "no config vars set")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tUPDATED\tPROVIDED BY")
			if sec != nil {
				for _, v := range configvars.List(sec) {
					when := "-"
					if t, err := time.Parse(time.RFC3339, v.UpdatedAt); err == nil {
						when = age(metav1.NewTime(t))
					}
					note := "-"
					if global != nil {
						if _, shadowed := global.Data[v.Name]; shadowed {
							note = "- (overrides global)"
						}
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", v.Name, when, note)
				}
			}
			if global != nil {
				for _, v := range configvars.List(global) {
					if sec != nil {
						if _, shadowed := sec.Data[v.Name]; shadowed {
							continue // the project's own value is in effect
						}
					}
					when := "-"
					if t, err := time.Parse(time.RFC3339, v.UpdatedAt); err == nil {
						when = age(metav1.NewTime(t))
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", v.Name, when, "cluster")
				}
			}
			if bound != nil {
				providers := map[string]string{}
				_ = json.Unmarshal([]byte(bound.Annotations[shpyrdv1.AnnotationBindingProviders]), &providers)
				names := make([]string, 0, len(bound.Data))
				for k := range bound.Data {
					names = append(names, k)
				}
				sort.Strings(names)
				for _, k := range names {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", k, "-", firstNonEmpty(providers[k], "binding"))
				}
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
	ac.audit(ctx, name, "config.set", name, configDetail(set, unset))
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
				pinned := ac.singleInstanceVolumes(ctx, a)
				for proc, n := range changes {
					if v, ok := pinned[proc]; ok && n > 1 {
						return fmt.Errorf("%s mounts single-instance volume %q and can run 1 instance (a shared volume allows more)", proc, v)
					}
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
			ac.audit(ctx, name, "scale", name, strings.Join(args, " "))
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
	cmd.Flags().Int64VarP(&tail, "tail", "n", 100, "number of recent lines per instance (-1 for all)")
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
			ac.audit(ctx, name, "rollback", name, fmt.Sprintf("to v%d", target.Number))
			img := target.Image
			note := fmt.Sprintf("Rollback to v%d", target.Number)
			sizesOf := target.Sizes
			bindingsOf := target.Bindings
			updated, err := ac.updateApp(ctx, name, func(a *shpyrdv1.App) error {
				a.Spec.Image = img
				a.Spec.Bindings = append([]shpyrdv1.Binding(nil), bindingsOf...)
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

// configDetail names the variables touched by a config change, never values.
func configDetail(set map[string]string, unset []string) string {
	var parts []string
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		parts = append(parts, "set "+strings.Join(keys, ", "))
	}
	if len(unset) > 0 {
		parts = append(parts, "unset "+strings.Join(unset, ", "))
	}
	return strings.Join(parts, "; ")
}

// healthLabel summarises a process's probe config for `projects info`.
func healthLabel(processName string, p shpyrdv1.Process) string {
	hc := p.HealthCheck
	if hc != nil && hc.Disabled {
		return "disabled"
	}
	port := int32(0)
	if p.Port != nil {
		port = *p.Port
	} else if processName == "web" {
		port = shpyrdv1.DefaultWebPort
	}
	switch {
	case hc != nil && len(hc.Command) > 0:
		return "command " + strings.Join(hc.Command[:1], "") + " (custom)"
	case hc != nil && hc.Path != "":
		return "HTTP " + hc.Path + " (custom)"
	case hc != nil && hc.TCP:
		return fmt.Sprintf("TCP port %d (custom)", port)
	case processName == "web" && port > 0:
		return fmt.Sprintf("HTTP / on port %d (default)", port)
	case port > 0:
		return fmt.Sprintf("TCP port %d (default)", port)
	default:
		return "" // workers without a port: nothing to print
	}
}

// shpyrd redeploy: new instances of the current release, or the same source
// built again after a failed build (or with --rebuild). No release is created.
func newRedeployCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		rebuild bool
		noWait  bool
	)
	cmd := &cobra.Command{
		Use:   "redeploy",
		Short: "Try the current release again: restart its instances, or build the same source again after a failed build",
		Long: `Redeploy creates no release. With a healthy or unhealthy release it starts
new instances of the current one (a rolling restart). When the last build
failed, or with --rebuild, it builds the same source again; the release
that results is a normal deploy.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			out := cmd.OutOrStdout()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, out)
			if err != nil {
				return err
			}
			app, err := ac.getApp(ctx, name)
			if err != nil {
				return err
			}
			action := "restart"
			buildFailed := app.HasSource() && app.Spec.Image == "" && meta.IsStatusConditionFalse(app.Status.Conditions, shpyrdv1.ConditionBuilt)
			if rebuild || buildFailed {
				action = "rebuild"
				if !app.HasSource() || app.Spec.Image != "" {
					return errors.New("nothing to build: the project runs a pinned image")
				}
			} else if app.Status.Phase == shpyrdv1.PhaseBuilding {
				return errors.New("a build is running; wait for it to finish")
			} else if app.CurrentRelease() == nil {
				return errors.New("nothing to restart: no release yet")
			}
			now := time.Now().UTC().Format(time.RFC3339)
			updated, err := ac.updateApp(ctx, name, func(a *shpyrdv1.App) error {
				if a.Annotations == nil {
					a.Annotations = map[string]string{}
				}
				if action == "rebuild" {
					a.Annotations[shpyrdv1.AnnotationRebuildAt] = now
				} else {
					a.Annotations[shpyrdv1.AnnotationRestartedAt] = now
				}
				return nil
			})
			if err != nil {
				return err
			}
			ac.audit(ctx, name, "redeploy", name, action)
			if action == "rebuild" {
				fmt.Fprintf(out, "==> Building %s again from the same source\n", name)
			} else if cur := app.CurrentRelease(); cur != nil {
				fmt.Fprintf(out, "==> Restarting the instances of %s v%d\n", name, cur.Number)
			}
			if noWait {
				return nil
			}
			if action == "rebuild" {
				build, err := ac.waitForNewBuild(ctx, name, app.Status.LatestBuild, time.Now().Add(-time.Minute), 3*time.Minute)
				if err != nil {
					return err
				}
				if err := ac.followBuild(ctx, appNamespace(name), build); err != nil {
					return err
				}
			}
			final, err := ac.waitRunning(ctx, name, updated.Generation, 10*time.Minute)
			if err != nil {
				return err
			}
			if rel := final.CurrentRelease(); rel != nil {
				fmt.Fprintf(out, "\n%s is running v%d.\n", name, rel.Number)
			}
			return nil
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "build the same source again even if the last build succeeded")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return without waiting")
	return cmd
}

// shpyrd projects exposure <project> internal|external (RFC-0036).
func newExposureCmd(g *globalFlags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "exposure internal|external",
		Short: "Change which front door serves the project (external: public LB, internal: private LB)",
		Long: `Exposure is a release-free operation: the Ingress is re-rendered immediately,
DNS records follow on the provider's next sync, certificates are already
covered by the platform's wildcard.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			exposure := strings.ToLower(args[0])
			if exposure != "external" && exposure != "internal" {
				return fmt.Errorf("exposure must be external or internal")
			}
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			body, _ := json.Marshal(map[string]string{"exposure": exposure})
			raw, err := serverRequest(ctx, ac.k, "PUT", "api/projects/"+name+"/exposure", body, "application/json")
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &map[string]interface{}{}); err != nil {
				if len(raw) > 0 {
					_ = raw // response acknowledged
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s exposure set to %s\n", name, exposure)
			return nil
		},
	}
	appFlag(cmd, &appName)
	return cmd
}

package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/project"
)

func newAppsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "projects",
		Short:   "Create, list and inspect projects",
		Aliases: []string{"project", "apps", "app"},
	}
	cmd.AddCommand(newAppsCreateCmd(g), newAppsListCmd(g), newAppsInfoCmd(g), newAppsRenameCmd(g), newAppsDestroyCmd(g))
	return cmd
}

func newAppsCreateCmd(g *globalFlags) *cobra.Command {
	var (
		domains []string
		save    bool
		slug    string
		public  bool
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a project",
		Long: `Create a project. The name is free text ("My Shop"); its slug (my-shop) is
derived from it and identifies the project in the CLI, in URLs and in the
hostname <slug>.<cluster domain>. Pass --slug to choose it.

New projects ask visitors to sign in: only people with a role on the project
(teams with the user role, its developers and admins) can open the app, and
the app receives who they are. --public makes it a site anyone can open;
` + "`shpyrd access`" + ` changes it later.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if slug == "" {
				var err error
				if slug, err = project.Slug(name); err != nil {
					return err
				}
			} else if err := project.ValidateSlug(slug); err != nil {
				return err
			}
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name:   appNamespace(slug),
				Labels: project.NamespaceLabels(project.DefaultWorkspace, slug),
			}}
			if err := ac.c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create namespace: %w", err)
			}
			access := shpyrdv1.AccessAuthenticated
			if public {
				access = shpyrdv1.AccessPublic
			}
			app := &shpyrdv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: slug, Namespace: ns.Name},
				Spec:       shpyrdv1.AppSpec{Domains: domains, Access: access},
			}
			project.SetDisplayName(app, name)
			if err := ac.c.Create(ctx, app); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("project %q already exists; choose another slug, for example --slug %s-2", slug, slug)
				}
				return fmt.Errorf("create project: %w", err)
			}
			ac.audit(ctx, slug, "project.create", project.Label(app), "")
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Created project %s\n", project.Label(app))
			if public {
				fmt.Fprintln(out, "Anyone on the internet can open it (public). `shpyrd access set authenticated` closes it.")
			} else {
				fmt.Fprintln(out, "Visitors must sign in; grant a team the user role to let it in (`shpyrd members add`), or `shpyrd access set public` for a site.")
			}
			if save {
				if err := os.WriteFile("shpyrd.yaml", []byte("project: "+slug+"\n"), 0o644); err != nil {
					return err
				}
				fmt.Fprintln(out, "Wrote shpyrd.yaml")
				fmt.Fprintln(out, "Next: shpyrd deploy")
			} else {
				fmt.Fprintf(out, "Next: shpyrd deploy --project %s   (or add `project: %s` to shpyrd.yaml)\n", slug, slug)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "identifier to use instead of the one derived from the name")
	cmd.Flags().StringSliceVar(&domains, "domain", nil, "custom domains served in addition to <slug>.<cluster domain> (see `shpyrd domains`)")
	cmd.Flags().BoolVar(&save, "save", false, "write shpyrd.yaml in the current directory")
	cmd.Flags().BoolVar(&public, "public", false, "anyone can open the app (a site); the default asks visitors to sign in")
	return cmd
}

func newAppsRenameCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <project> <new name>",
		Short: "Change the display name of a project (the slug never changes)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateAppName(args[0]); err != nil {
				return err
			}
			name := strings.TrimSpace(args[1])
			if name == "" {
				return errors.New("the new name must not be empty")
			}
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			app, err := ac.getApp(ctx, args[0])
			if err != nil {
				return err
			}
			project.SetDisplayName(app, name)
			if err := ac.c.Update(ctx, app); err != nil {
				return err
			}
			ac.audit(ctx, app.Name, "project.rename", project.Label(app), "")
			fmt.Fprintf(cmd.OutOrStdout(), "Renamed project %s\n", project.Label(app))
			return nil
		},
	}
}

func newAppsListCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List projects",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			var list shpyrdv1.AppList
			if err := ac.c.List(ctx, &list); err != nil {
				return err
			}
			sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PROJECT\tNAME\tPHASE\tRELEASE\tURL\tAGE")
			for i := range list.Items {
				a := &list.Items[i]
				rel := "-"
				if r := a.CurrentRelease(); r != nil {
					rel = fmt.Sprintf("v%d", r.Number)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, project.DisplayName(a), firstNonEmpty(a.Status.Phase, "Pending"), rel, a.Status.URL, age(a.CreationTimestamp))
			}
			return tw.Flush()
		},
	}
}

func newAppsInfoCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "info <project>",
		Short: "Show a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			app, err := ac.getApp(ctx, args[0])
			if err != nil {
				return err
			}
			printAppInfo(cmd, app)
			var vols shpyrdv1.VolumeList
			_ = ac.c.List(ctx, &vols, client.InNamespace(app.Namespace))
			printResources(cmd, app, vols.Items)
			return nil
		},
	}
}

// printResources lists every resource of the project (RFC-0003): the app
// itself, its attached resources and the volumes.
func printResources(cmd *cobra.Command, app *shpyrdv1.App, vols []shpyrdv1.Volume) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Resources:")
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", "App", app.Name, firstNonEmpty(app.Status.Phase, "Pending"), firstNonEmpty(app.Status.URL, "-"))
	for _, b := range app.Spec.Bindings {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", b.Kind, b.Name, "attached", "config vars "+strings.ToUpper(firstNonEmpty(b.Prefix, "<default>"))+"_*")
	}
	for _, v := range vols {
		mode := "single-instance"
		if v.Shared() {
			mode = "shared"
		}
		detail := fmt.Sprintf("%s %s", v.Spec.Size.String(), mode)
		if len(v.Status.MountedBy) > 0 {
			detail += ", mounted by " + strings.Join(v.Status.MountedBy, ", ")
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", "Volume", v.Name, firstNonEmpty(v.Status.Phase, "Pending"), detail)
	}
	_ = tw.Flush()
}

func printAppInfo(cmd *cobra.Command, app *shpyrdv1.App) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Project:    %s\n", project.Label(app))
	fmt.Fprintf(out, "Phase:      %s\n", firstNonEmpty(app.Status.Phase, "Pending"))
	if app.Status.Message != "" {
		fmt.Fprintf(out, "Message:    %s\n", app.Status.Message)
	}
	if app.Status.URL != "" {
		fmt.Fprintf(out, "URL:        %s\n", app.Status.URL)
	}
	switch {
	case app.Spec.Image != "":
		fmt.Fprintf(out, "Digest:     %s (pinned)\n", digest(app.Spec.Image))
	case app.Status.Image != "":
		fmt.Fprintf(out, "Digest:     %s\n", digest(app.Status.Image))
	}
	if app.Spec.Source != nil {
		switch {
		case app.Spec.Source.Git != nil:
			fmt.Fprintf(out, "Source:     git %s @ %s\n", app.Spec.Source.Git.URL, firstNonEmpty(app.Spec.Source.Git.Revision, "main"))
		case app.Spec.Source.Blob != nil:
			fmt.Fprintf(out, "Source:     archive %s (%s)\n", short(app.Spec.Source.Blob.SHA256), firstNonEmpty(app.Spec.Source.Blob.Ref, "local"))
		}
		if app.Spec.Source.SubPath != "" {
			fmt.Fprintf(out, "Path:       %s\n", app.Spec.Source.SubPath)
		}
	}
	// Health check summary per process (RFC-0019).
	if len(app.Spec.Processes) > 0 {
		pnames := make([]string, 0, len(app.Spec.Processes))
		for n := range app.Spec.Processes {
			pnames = append(pnames, n)
		}
		sort.Strings(pnames)
		for _, n := range pnames {
			p := app.Spec.Processes[n]
			probe := healthLabel(n, p)
			if probe != "" {
				fmt.Fprintf(out, "Health:     %s: %s\n", n, probe)
			}
		}
	}
	if len(app.Status.Processes) > 0 {
		names := make([]string, 0, len(app.Status.Processes))
		for n := range app.Status.Processes {
			names = append(names, n)
		}
		sort.Strings(names)
		var parts []string
		for _, n := range names {
			p := app.Status.Processes[n]
			part := fmt.Sprintf("%s %d/%d", n, p.Ready, p.Desired)
			if p.Failing > 0 {
				part += fmt.Sprintf(" (%d failing: %s)", p.Failing, p.Reason)
			}
			parts = append(parts, part)
		}
		fmt.Fprintf(out, "Processes:  %s\n", strings.Join(parts, ", "))
	}
	if n := len(app.Status.Releases); n > 0 {
		fmt.Fprintln(out, "Releases:")
		start := n - 5
		if start < 0 {
			start = 0
		}
		for _, r := range app.Status.Releases[start:] {
			fmt.Fprintf(out, "  v%-3d %-9s %s\n", r.Number, age(r.CreatedAt)+" ago", r.Description)
		}
	}
}

func newAppsDestroyCmd(g *globalFlags) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "destroy <project>",
		Short: "Delete a project and everything in it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			app, err := ac.getApp(ctx, name)
			if err != nil {
				return err
			}
			var vols shpyrdv1.VolumeList
			_ = ac.c.List(ctx, &vols, client.InNamespace(appNamespace(name)))
			if len(vols.Items) > 0 {
				var names []string
				for _, v := range vols.Items {
					names = append(names, fmt.Sprintf("%s (%s)", v.Name, v.Spec.Size.String()))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Warning: this deletes the data on volume(s) %s.\n", strings.Join(names, ", "))
			}
			if !confirm(cmd, yes, fmt.Sprintf("Delete project %s with all its resources?", project.Label(app)), false) {
				return fmt.Errorf("aborted")
			}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: appNamespace(name)}}
			if err := ac.c.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			ac.auditCluster(ctx, "project.destroy", project.Label(app), "")
			fmt.Fprintf(cmd.OutOrStdout(), "Deleting project %s...\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

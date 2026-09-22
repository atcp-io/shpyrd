package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

func newAppsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "projects",
		Short:   "Create, list and inspect projects",
		Aliases: []string{"project", "apps", "app"},
	}
	cmd.AddCommand(newAppsCreateCmd(g), newAppsListCmd(g), newAppsInfoCmd(g), newAppsDestroyCmd(g))
	return cmd
}

func newAppsCreateCmd(g *globalFlags) *cobra.Command {
	var (
		domains []string
		save    bool
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a project (its namespace and app resource)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := validateAppName(name); err != nil {
				return err
			}
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: appNamespace(name),
				Labels: map[string]string{
					shpyrdv1.LabelApp:       name,
					shpyrdv1.LabelManagedBy: "shpyrd",
				},
			}}
			if err := ac.c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create namespace: %w", err)
			}
			app := &shpyrdv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
				Spec:       shpyrdv1.AppSpec{Domains: domains},
			}
			if err := ac.c.Create(ctx, app); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("project %q already exists", name)
				}
				return fmt.Errorf("create project: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Created project %s\n", name)
			if save {
				if err := os.WriteFile("shpyrd.yaml", []byte("project: "+name+"\n"), 0o644); err != nil {
					return err
				}
				fmt.Fprintln(out, "Wrote shpyrd.yaml")
				fmt.Fprintln(out, "Next: shpyrd deploy")
			} else {
				fmt.Fprintf(out, "Next: shpyrd deploy --project %s   (or add `project: %s` to shpyrd.yaml)\n", name, name)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&domains, "domain", nil, "extra hostnames for the web process (default <name>.<cluster domain>)")
	cmd.Flags().BoolVar(&save, "save", false, "write shpyrd.yaml in the current directory")
	return cmd
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
			fmt.Fprintln(tw, "NAME\tPHASE\tRELEASE\tURL\tAGE")
			for _, a := range list.Items {
				rel := "-"
				if r := a.CurrentRelease(); r != nil {
					rel = fmt.Sprintf("v%d", r.Number)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Name, firstNonEmpty(a.Status.Phase, "Pending"), rel, a.Status.URL, age(a.CreationTimestamp))
			}
			return tw.Flush()
		},
	}
}

func newAppsInfoCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>",
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
			return nil
		},
	}
}

func printAppInfo(cmd *cobra.Command, app *shpyrdv1.App) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Project:    %s\n", app.Name)
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
		Use:   "destroy <name>",
		Short: "Delete a project and everything in it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if _, err := ac.getApp(ctx, name); err != nil {
				return err
			}
			if !yes {
				fmt.Fprintf(cmd.OutOrStdout(), "Delete project %q with all its resources? [y/N] ", name)
				var answer string
				fmt.Fscanln(os.Stdin, &answer)
				if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
					return fmt.Errorf("aborted")
				}
			}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: appNamespace(name)}}
			if err := ac.c.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleting %s...\n", ns.Name)
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

package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"shpyrd/pkg/ext"
	"shpyrd/pkg/ext/all"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
)

// Kubeconfig implements ext.CLIGlobals.
func (g *globalFlags) Kubeconfig() string { return g.kubeconfig }

// Context implements ext.CLIGlobals.
func (g *globalFlags) Context() string { return g.kubeCtx }

// extensionComponents resolves extension names to installer components.
func extensionComponents(names []string) ([]install.ExtensionComponent, error) {
	var out []install.ExtensionComponent
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		x := ext.Find(all.All(), name)
		if x == nil {
			return nil, fmt.Errorf("unknown extension %q (available: %s)", name, strings.Join(ext.Names(all.All()), ", "))
		}
		ec := install.ExtensionComponent{Extension: name}
		if c := x.Component(); c != nil {
			ec.Component, ec.Runlevel = c.Name, c.Runlevel
		}
		out = append(out, ec)
	}
	return out, nil
}

// recordedExtensions reads the enabled extensions from the install record.
func recordedExtensions(ctx context.Context, k *kube.Client) []string {
	info, err := install.ReadInstallInfo(ctx, k, "")
	if err != nil || info == nil {
		return nil
	}
	return splitList(info.Vars[install.VarExtensions])
}

func splitList(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mergeExtensions applies --enable/--disable to the recorded list.
func mergeExtensions(recorded, enable, disable []string) []string {
	set := map[string]bool{}
	for _, n := range recorded {
		set[n] = true
	}
	for _, n := range enable {
		set[n] = true
	}
	for _, n := range disable {
		delete(set, n)
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func newExtensionsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "extensions",
		Aliases: []string{"extension", "ext"},
		Short:   "Optional platform capabilities, enabled per cluster",
		Long: `Extensions add capabilities to a cluster: a login issuer, databases, caches...
Each one is compiled into shpyrd and switched on per cluster; enabling
installs its component and restarts the server with it, disabling removes
the component (refused while resources of the extension exist).

  shpyrd extensions list
  shpyrd extensions enable auth-local
  shpyrd extensions disable auth-local`,
	}
	cmd.AddCommand(newExtensionsListCmd(g), newExtensionsEnableCmd(g), newExtensionsDisableCmd(g))
	return cmd
}

func newExtensionsListCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List extensions and whether they are enabled on the current cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			enabled := map[string]bool{}
			if k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx}); err == nil {
				for _, n := range recordedExtensions(ctx, k) {
					enabled[n] = true
				}
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS\tCOMPONENT\tDESCRIPTION")
			for _, x := range all.All() {
				status := "disabled"
				if enabled[x.Name()] {
					status = "enabled"
				}
				comp := "-"
				if c := x.Component(); c != nil {
					comp = c.Name
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", x.Name(), status, comp, x.Description())
			}
			return tw.Flush()
		},
	}
}

// extensionEngine builds an installer from the recorded profile and
// variables with the given extension set, so enable/disable never changes
// the domain or ports the cluster was installed with.
func extensionEngine(ctx context.Context, cmd *cobra.Command, k *kube.Client, names []string, only []string, set []string) (*install.Engine, error) {
	info, err := install.ReadInstallInfo(ctx, k, "")
	if err != nil || info == nil {
		return nil, errors.New("this cluster has no shpyrd install record; run `shpyrd cluster init` first")
	}
	vars := map[string]string{}
	for k, v := range info.Vars {
		switch k {
		case install.VarExtensions, install.VarDashboardURL, install.VarAuthURL, install.VarProfile, install.VarVersion, install.VarSystemNS:
			continue // derived or built in
		}
		vars[k] = v
	}
	for _, kv := range set {
		key, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, "SHPYRD_") {
			return nil, fmt.Errorf("--set expects SHPYRD_NAME=value, got %q", kv)
		}
		vars[key] = v
	}
	comps, err := extensionComponents(names)
	if err != nil {
		return nil, err
	}
	return install.New(k, install.Options{
		Profile:    firstNonEmpty(info.Profile, "local"),
		Vars:       vars,
		Only:       only,
		Version:    Version,
		Extensions: comps,
		Reporter:   &consoleReporter{out: cmd.OutOrStdout()},
	})
}

func newExtensionsEnableCmd(g *globalFlags) *cobra.Command {
	var set []string
	cmd := &cobra.Command{
		Use:   "enable <name>",
		Short: "Enable an extension: install its component and restart the server with it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name := args[0]
			x := ext.Find(all.All(), name)
			if x == nil {
				return fmt.Errorf("unknown extension %q (available: %s)", name, strings.Join(ext.Names(all.All()), ", "))
			}
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			names := mergeExtensions(recordedExtensions(ctx, k), []string{name}, nil)
			// Apply the extension's component and the server (its
			// SHPYRD_EXTENSIONS env changes, so it rolls out).
			only := []string{"shpyrd"}
			if c := x.Component(); c != nil {
				only = append([]string{c.Name}, only...)
			}
			eng, err := extensionEngine(ctx, cmd, k, names, only, set)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Enabling %s: %s\n", name, x.Description())
			if err := eng.Apply(ctx); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nExtension %s is enabled.\n", name)
			for _, hint := range enableHints(x, eng.Vars()) {
				fmt.Fprintln(cmd.OutOrStdout(), hint)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&set, "set", nil, "override a variable, e.g. --set SHPYRD_SERVER_IMAGE=...")
	return cmd
}

// enableHints tells the user what to do next for extensions that need it.
func enableHints(x ext.Extension, vars map[string]string) []string {
	if x.Name() == "auth-local" {
		return []string{
			"Create the first account:  shpyrd users add you@example.com",
			"Then sign in at " + vars[install.VarDashboardURL] + " with \"Email and password\".",
		}
	}
	return nil
}

func newExtensionsDisableCmd(g *globalFlags) *cobra.Command {
	var (
		set []string
		yes bool
	)
	cmd := &cobra.Command{
		Use:   "disable <name>",
		Short: "Disable an extension: remove its component (refused while its resources exist)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name := args[0]
			x := ext.Find(all.All(), name)
			if x == nil {
				return fmt.Errorf("unknown extension %q", name)
			}
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			recorded := recordedExtensions(ctx, k)
			if !contains(recorded, name) {
				return fmt.Errorf("extension %s is not enabled on this cluster", name)
			}
			if in, err := resourcesInUse(ctx, k, x); err != nil {
				return err
			} else if len(in) > 0 {
				return fmt.Errorf("cannot disable %s: %s still exist; delete them first", name, strings.Join(in, ", "))
			}
			if !yes {
				return fmt.Errorf("this removes the %s component from the cluster; re-run with --yes to confirm", name)
			}
			// Remove the component with the current (still enabled) set so
			// its manifests render, then re-apply the server without it.
			eng, err := extensionEngine(ctx, cmd, k, recorded, nil, set)
			if err != nil {
				return err
			}
			if c := x.Component(); c != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Removing component %s\n", c.Name)
				if err := eng.Remove(ctx, c.Name); err != nil {
					return err
				}
			}
			after, err := extensionEngine(ctx, cmd, k, mergeExtensions(recorded, nil, []string{name}), []string{"shpyrd"}, set)
			if err != nil {
				return err
			}
			if err := after.Apply(ctx); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nExtension %s is disabled.\n", name)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&set, "set", nil, "override a variable")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm")
	return cmd
}

// resourcesInUse counts the extension's resource kinds across the cluster.
func resourcesInUse(ctx context.Context, k *kube.Client, x ext.Extension) ([]string, error) {
	var out []string
	for _, t := range x.Types() {
		gvr := schema.GroupVersionResource{Group: t.Group, Version: t.Version, Resource: t.Resource}
		list, err := k.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue // CRD gone already
		}
		if n := len(list.Items); n > 0 {
			out = append(out, fmt.Sprintf("%d %s", n, t.Kind))
		}
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/configvars"
	"shpyrd/pkg/install"
)

// Global config vars (RFC-0016): `shpyrd globals set|unset|list`, a platform
// admin's counterpart of `shpyrd secrets`.

func newGlobalsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "globals",
		Short: "Manage config vars every project receives",
		Long: `Global config vars are set once by a platform admin and injected into every
process of every project, first in the environment so a project's own config
var of the same name wins and attached resources win over both. Changing them
creates a "Global config change" release in every project that has not opted
out (shpyrd.yaml: globals: false, or globals: {exclude: [NAME]}). Values are
write-only: they are never printed back.

  shpyrd globals set OPENAI_API_KEY=sk-... REGION=eu
  shpyrd globals unset REGION
  shpyrd globals list`,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "set KEY=VALUE [KEY=VALUE...]",
		Short: "Set global config vars",
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
			return mutateGlobals(g, cmd, set, nil)
		},
	}, &cobra.Command{
		Use:   "unset KEY [KEY...]",
		Short: "Remove global config vars",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return mutateGlobals(g, cmd, nil, args)
		},
	}, &cobra.Command{
		Use:     "list",
		Short:   "List global config var names (never values)",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			sec := &corev1.Secret{}
			if err := ac.c.Get(ctx, globalsKey(), sec); err != nil {
				if apierrors.IsNotFound(err) {
					fmt.Fprintln(cmd.OutOrStdout(), "no global config vars set")
					return nil
				}
				return err
			}
			vars := configvars.List(sec)
			if len(vars) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no global config vars set")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tUPDATED")
			for _, v := range vars {
				when := "-"
				if t, err := time.Parse(time.RFC3339, v.UpdatedAt); err == nil {
					when = age(metav1.NewTime(t))
				}
				fmt.Fprintf(tw, "%s\t%s\n", v.Name, when)
			}
			return tw.Flush()
		},
	})
	return cmd
}

func globalsKey() types.NamespacedName {
	return types.NamespacedName{Namespace: install.DefaultSystemNamespace, Name: shpyrdv1.GlobalEnvSecretName}
}

func mutateGlobals(g *globalFlags, cmd *cobra.Command, set map[string]string, unset []string) error {
	ctx := signalContext()
	ac, err := newAppClient(g, cmd.OutOrStdout())
	if err != nil {
		return err
	}
	sec := &corev1.Secret{}
	create := false
	if err := ac.c.Get(ctx, globalsKey(), sec); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		create = true
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: shpyrdv1.GlobalEnvSecretName, Namespace: install.DefaultSystemNamespace, Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}},
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
	if len(set) > 0 {
		ac.auditCluster(ctx, "globals.set", "global config vars", configDetail(set, nil))
	}
	if len(unset) > 0 {
		ac.auditCluster(ctx, "globals.unset", "global config vars", configDetail(nil, unset))
	}
	out := cmd.OutOrStdout()
	names := make([]string, 0, len(configvars.List(sec)))
	for _, v := range configvars.List(sec) {
		names = append(names, v.Name)
	}
	fmt.Fprintf(out, "Global config vars: %s\n", firstNonEmpty(strings.Join(names, ", "), "(none)"))
	var apps shpyrdv1.AppList
	if err := ac.c.List(ctx, &apps); err == nil {
		n := 0
		for _, a := range apps.Items {
			if a.Spec.Globals == nil || !a.Spec.Globals.Disabled {
				n++
			}
		}
		if n > 0 {
			fmt.Fprintf(out, "Releasing the change to %d project(s)...\n", n)
		}
	}
	return nil
}

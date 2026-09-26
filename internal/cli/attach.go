package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/ext/all"
)

// Attaching a resource (RFC-0003) adds a binding to the app: the resource's
// connection details become config vars (DATABASE_URL, REDIS_URL...) and a
// config release rolls the processes once the resource is ready.

var prefixRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,30}$`)

func newAttachCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		kind    string
		prefix  string
	)
	cmd := &cobra.Command{
		Use:   "attach <resource>",
		Short: "Attach a project resource (Postgres, Redis...) to the app as config vars",
		Long: `Attach a resource of the project to its app. The resource's connection
details are injected as config vars named after a prefix (DATABASE_URL,
DATABASE_HOST... for Postgres; REDIS_URL... for Redis) and every attach or
detach is a release that can be rolled back.

  shpyrd attach db                       # Postgres db -> DATABASE_*
  shpyrd attach cache --prefix SESSIONS  # Redis cache -> SESSIONS_URL, SESSIONS_HOST...
  shpyrd detach db`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			if prefix != "" && !prefixRe.MatchString(prefix) {
				return errors.New("--prefix must be letters, digits and underscores")
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if _, err := ac.getApp(ctx, name); err != nil {
				return err
			}
			t, err := ac.findResource(ctx, appNamespace(name), args[0], kind)
			if err != nil {
				return err
			}
			app, err := ac.updateApp(ctx, name, func(a *shpyrdv1.App) error {
				for _, b := range a.Spec.Bindings {
					if b.Kind == t.Kind && b.Name == args[0] {
						return fmt.Errorf("%s %s is already attached", t.Kind, args[0])
					}
				}
				a.Spec.Bindings = append(a.Spec.Bindings, shpyrdv1.Binding{Kind: t.Kind, Name: args[0], Prefix: strings.ToUpper(prefix)})
				if a.Annotations == nil {
					a.Annotations = map[string]string{}
				}
				a.Annotations[shpyrdv1.AnnotationReleaseNote] = fmt.Sprintf("Attach %s %s", t.Kind, args[0])
				return nil
			})
			if err != nil {
				return err
			}
			ac.audit(ctx, name, "attach", t.Kind+" "+args[0], prefix)
			p := strings.ToUpper(firstNonEmpty(prefix, defaultPrefix(t.Kind)))
			fmt.Fprintf(cmd.OutOrStdout(), "Attached %s %s to %s: config vars %s_URL, %s_HOST, ... (values are never shown)\n", t.Kind, args[0], name, p, p)
			if app.Status.Image != "" {
				fmt.Fprintln(cmd.OutOrStdout(), "Releasing with the new configuration (the app waits while the resource is still provisioning).")
			}
			return nil
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().StringVar(&kind, "kind", "", "resource kind when the name is ambiguous (Postgres, Redis)")
	cmd.Flags().StringVar(&prefix, "prefix", "", "config var prefix (default per kind: DATABASE, REDIS)")
	return cmd
}

func newDetachCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		kind    string
	)
	cmd := &cobra.Command{
		Use:   "detach <resource>",
		Short: "Detach a resource from the app (its config vars are removed)",
		Args:  cobra.ExactArgs(1),
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
			var removed *shpyrdv1.Binding
			if _, err := ac.updateApp(ctx, name, func(a *shpyrdv1.App) error {
				kept := a.Spec.Bindings[:0]
				var matches []shpyrdv1.Binding
				for _, b := range a.Spec.Bindings {
					if b.Name == args[0] && (kind == "" || strings.EqualFold(b.Kind, kind)) {
						matches = append(matches, b)
						continue
					}
					kept = append(kept, b)
				}
				switch len(matches) {
				case 0:
					return fmt.Errorf("nothing named %q is attached to %s", args[0], name)
				case 1:
					removed = &matches[0]
				default:
					return fmt.Errorf("several resources named %q are attached; pass --kind", args[0])
				}
				a.Spec.Bindings = kept
				if a.Annotations == nil {
					a.Annotations = map[string]string{}
				}
				a.Annotations[shpyrdv1.AnnotationReleaseNote] = fmt.Sprintf("Detach %s %s", removed.Kind, removed.Name)
				return nil
			}); err != nil {
				return err
			}
			ac.audit(ctx, name, "detach", removed.Kind+" "+removed.Name, "")
			fmt.Fprintf(cmd.OutOrStdout(), "Detached %s %s from %s; its config vars are removed in the next release.\n", removed.Kind, removed.Name, name)
			return nil
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().StringVar(&kind, "kind", "", "resource kind when the name is ambiguous")
	return cmd
}

// findResource locates a bindable resource by name (and kind when given).
func (a *appClient) findResource(ctx context.Context, namespace, name, kind string) (ext.ResourceType, error) {
	var found []ext.ResourceType
	for _, t := range all.BindableTypes(all.All()) {
		if kind != "" && !strings.EqualFold(t.Kind, kind) {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: t.Group, Version: t.Version, Kind: t.Kind})
		if err := a.c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, u); err == nil {
			found = append(found, t)
		} else if !apierrors.IsNotFound(err) && !strings.Contains(err.Error(), "no matches for kind") {
			return ext.ResourceType{}, err
		}
	}
	switch len(found) {
	case 0:
		var names []string
		for _, t := range all.BindableTypes(all.All()) {
			names = append(names, t.Kind)
		}
		return ext.ResourceType{}, fmt.Errorf("no attachable resource named %q in this project (kinds: %s); create one with `shpyrd pg create` or `shpyrd redis create`", name, strings.Join(names, ", "))
	case 1:
		return found[0], nil
	default:
		var kinds []string
		for _, t := range found {
			kinds = append(kinds, t.Kind)
		}
		return ext.ResourceType{}, fmt.Errorf("%q exists as %s: pass --kind", name, strings.Join(kinds, " and "))
	}
}

func defaultPrefix(kind string) string {
	switch kind {
	case "Postgres":
		return "DATABASE"
	case "Redis":
		return "REDIS"
	}
	return strings.ToUpper(kind)
}

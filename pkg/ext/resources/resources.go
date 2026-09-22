// Package resources holds helpers shared by the data store extensions' CLIs:
// creating, listing and deleting project resources with the kubeconfig,
// and refusing to delete what an app still uses.
package resources

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/kube"
)

// Namespace of a project.
func Namespace(project string) string { return "app-" + project }

// Connect builds the controller-runtime client the CLIs use.
func Connect(g ext.CLIGlobals) (*kube.Client, client.Client, error) {
	k, err := kube.Connect(kube.Options{Kubeconfig: g.Kubeconfig(), Context: g.Context()})
	if err != nil {
		return nil, nil, err
	}
	c, err := k.ControllerClient()
	if err != nil {
		return nil, nil, err
	}
	return k, c, nil
}

// RequireProject fails with a helpful message when the project is missing.
func RequireProject(ctx context.Context, c client.Client, project string) error {
	app := &shpyrdv1.App{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: Namespace(project), Name: project}, app); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("project %q not found (see `shpyrd projects list`)", project)
		}
		return err
	}
	return nil
}

// BoundBy lists the apps of the namespace attaching kind/name.
func BoundBy(ctx context.Context, c client.Client, namespace, kind, name string) ([]string, error) {
	var apps shpyrdv1.AppList
	if err := c.List(ctx, &apps, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var out []string
	for _, a := range apps.Items {
		for _, b := range a.Spec.Bindings {
			if b.Kind == kind && b.Name == name {
				out = append(out, a.Name)
			}
		}
	}
	return out, nil
}

// CheckDeletable refuses deletion while bound unless forced.
func CheckDeletable(ctx context.Context, c client.Client, namespace, kind, name string, force bool) error {
	bound, err := BoundBy(ctx, c, namespace, kind, name)
	if err != nil {
		return err
	}
	if len(bound) > 0 && !force {
		return fmt.Errorf("%s %s is attached to %s: detach it first (`shpyrd detach %s`) or pass --force", kind, name, strings.Join(bound, ", "), name)
	}
	return nil
}

// ExtensionHint explains a missing controller.
func ExtensionHint(extension string) string {
	return fmt.Sprintf("the %s extension is not enabled on this cluster: run `shpyrd extensions enable %s`", extension, extension)
}

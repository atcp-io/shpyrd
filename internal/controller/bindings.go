package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
)

// Binder is the binding side of a resource kind (RFC-0002, RFC-0003): it
// turns a resource of the project into config vars for an app. Resource
// kinds register a Binder; the App controller never reads provider objects
// itself.
type Binder interface {
	// DefaultPrefix names the variables when the binding sets none
	// (DATABASE -> DATABASE_URL).
	DefaultPrefix() string
	// ConfigVars returns the variables of resource name in namespace, named
	// with prefix. It returns a *NotReadyError while the resource is still
	// provisioning and a NotFound API error when it does not exist.
	ConfigVars(ctx context.Context, c client.Client, namespace, name, prefix string) (map[string]string, error)
}

// NotReadyError says a bound resource exists but has no credentials yet.
type NotReadyError struct{ Msg string }

func (e *NotReadyError) Error() string { return e.Msg }

var binders = map[string]Binder{}

// RegisterBinder makes a resource kind attachable.
func RegisterBinder(kind string, b Binder) { binders[kind] = b }

// BindableKinds lists the kinds that can be attached.
func BindableKinds() []string {
	out := make([]string, 0, len(binders))
	for k := range binders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reconcileBindings renders the config vars of attached resources into the
// Secret <app>-bindings (owned by the app) and returns it; without bindings
// the Secret is removed and nil returned. Errors name the binding so the
// app status explains what is missing.
func (r *AppReconciler) reconcileBindings(ctx context.Context, app *shpyrdv1.App) (*corev1.Secret, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: app.BindingsSecretName(), Namespace: app.Namespace}}
	if len(app.Spec.Bindings) == 0 {
		if err := r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: secret.Name}, secret); err == nil {
			if err := r.deleteIfExists(ctx, secret); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	vars := map[string]string{}
	providedBy := map[string]string{}
	for _, b := range app.Spec.Bindings {
		binder, ok := binders[b.Kind]
		if !ok {
			return nil, fmt.Errorf("binding %s/%s: resources of kind %q cannot be attached (available: %s)", b.Kind, b.Name, b.Kind, firstNonEmpty(strings.Join(BindableKinds(), ", "), "none installed"))
		}
		prefix := strings.ToUpper(firstNonEmpty(b.Prefix, binder.DefaultPrefix()))
		values, err := binder.ConfigVars(ctx, r.Client, app.Namespace, b.Name, prefix)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("binding %s/%s: the resource does not exist in this project", b.Kind, b.Name)
			}
			return nil, fmt.Errorf("binding %s/%s: %w", b.Kind, b.Name, err)
		}
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if other, dup := providedBy[k]; dup {
				return nil, fmt.Errorf("bindings %s and %s/%s both provide %s: set a different prefix on one of them", other, b.Kind, b.Name, k)
			}
			providedBy[k] = b.Kind + "/" + b.Name
			vars[k] = values[k]
		}
	}

	providers, _ := json.Marshal(providedBy)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Type = corev1.SecretTypeOpaque
		secret.Labels = mergeMaps(secret.Labels, map[string]string{shpyrdv1.LabelApp: app.Name})
		// The Config tab shows which resource provides each variable.
		secret.Annotations = mergeMaps(secret.Annotations, map[string]string{shpyrdv1.AnnotationBindingProviders: string(providers)})
		secret.Data = map[string][]byte{}
		for k, v := range vars {
			secret.Data[k] = []byte(v)
		}
		return controllerutil.SetControllerReference(app, secret, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("write bindings secret: %w", err)
	}
	return secret, nil
}

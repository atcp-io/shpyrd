package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/configvars"
	"github.com/shpyrd-io/shpyrd/pkg/project"
)

// Global config vars (RFC-0016): a platform admin keeps them in Secret
// shpyrd-system/shpyrd-global-env; every project namespace gets a filtered
// mirror of the same name that the processes read first, so project vars
// and bound vars win. The mirror is owned by nobody (it outlives app
// changes and dies with the namespace) and carries the per-key metadata so
// the Config tab can show when each global was set.

// globalsFor filters the cluster Secret for one app: nil when the app opts
// out or nothing is set.
func globalsFor(app *shpyrdv1.App, global *corev1.Secret) map[string][]byte {
	if global == nil || len(global.Data) == 0 {
		return nil
	}
	g := app.Spec.Globals
	if g != nil && g.Disabled {
		return nil
	}
	excluded := map[string]bool{}
	if g != nil {
		for _, k := range g.Exclude {
			excluded[k] = true
		}
	}
	out := map[string][]byte{}
	for k, v := range global.Data {
		if !excluded[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reconcileGlobals refreshes the project's mirror of the global vars and
// returns it (nil when the project receives none).
func (r *AppReconciler) reconcileGlobals(ctx context.Context, app *shpyrdv1.App) (*corev1.Secret, error) {
	global := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Config.SystemNamespace, Name: shpyrdv1.GlobalEnvSecretName}, global); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get global config vars: %w", err)
		}
		global = nil
	}
	data := globalsFor(app, global)
	mirror := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: shpyrdv1.GlobalEnvSecretName, Namespace: app.Namespace}}
	if data == nil {
		if err := r.Get(ctx, client.ObjectKeyFromObject(mirror), mirror); err == nil {
			if err := r.deleteIfExists(ctx, mirror); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	// Keep only the metadata of the keys that made it through the filter.
	meta := map[string]json.RawMessage{}
	_ = json.Unmarshal([]byte(global.Annotations[configvars.AnnotationMeta]), &meta)
	for k := range meta {
		if _, ok := data[k]; !ok {
			delete(meta, k)
		}
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, mirror, func() error {
		mirror.Type = corev1.SecretTypeOpaque
		mirror.Labels = mergeMaps(mirror.Labels, map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", shpyrdv1.LabelProject: app.Name})
		mirror.Annotations = mergeMaps(mirror.Annotations, nil)
		if len(meta) > 0 {
			b, _ := json.Marshal(meta)
			mirror.Annotations[configvars.AnnotationMeta] = string(b)
		} else {
			delete(mirror.Annotations, configvars.AnnotationMeta)
		}
		mirror.Data = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("write global config vars mirror: %w", err)
	}
	return mirror, nil
}

// globalHash fingerprints the global vars a project receives.
func globalHash(mirror *corev1.Secret) string {
	if mirror == nil || len(mirror.Data) == 0 {
		return ""
	}
	h := sha256.New()
	keys := make([]string, 0, len(mirror.Data))
	for k := range mirror.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte("global:" + k + "="))
		h.Write(mirror.Data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// globalsDisabled says the app takes no global vars at all, so its
// Deployments do not even reference the mirror. Global vars are the
// operator's (RFC-0016) and reach the operator's own projects: apps of
// explicit workspaces (RFC-0033 phase 6) never receive them.
func globalsDisabled(app *shpyrdv1.App) bool {
	if workspaceOf(app) != project.DefaultWorkspace {
		return true
	}
	return app.Spec.Globals != nil && app.Spec.Globals.Disabled
}

// secretToApps maps Secret events to the Apps they concern: <app>-env to
// its App, the cluster-wide global vars to every App, a project's mirror
// to the Apps of that namespace (so a tampered mirror is repaired).
func (r *AppReconciler) secretToApps(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != shpyrdv1.GlobalEnvSecretName && !r.registrySecretChanged(obj) {
		return envSecretToApp(ctx, obj) // "<app>-env"; the global name ends in -env too
	}
	var apps shpyrdv1.AppList
	var opts []client.ListOption
	if obj.GetNamespace() != r.Config.SystemNamespace {
		opts = append(opts, client.InNamespace(obj.GetNamespace()))
	}
	if err := r.List(ctx, &apps, opts...); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(apps.Items))
	for _, a := range apps.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
	}
	return out
}

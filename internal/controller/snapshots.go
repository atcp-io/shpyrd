package controller

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/configvars"
)

// Releases carry their configuration, as on Heroku: when a release is
// recorded the config vars are copied into Secret <app>-release-v<N>, and a
// rollback restores that copy before pinning the release's build. Snapshots
// live next to the live Secret with the same access rules.

// snapshotRelease stores the current config vars for release n.
func (r *AppReconciler) snapshotRelease(ctx context.Context, app *shpyrdv1.App, n int, env *corev1.Secret) error {
	snap := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: app.ReleaseSnapshotName(n), Namespace: app.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, snap, func() error {
		snap.Type = corev1.SecretTypeOpaque
		snap.Labels = mergeMaps(snap.Labels, map[string]string{
			shpyrdv1.LabelApp:       app.Name,
			shpyrdv1.LabelRelease:   strconv.Itoa(n),
			shpyrdv1.LabelManagedBy: "shpyrd",
		})
		snap.Data = map[string][]byte{}
		if snap.Annotations == nil {
			snap.Annotations = map[string]string{}
		}
		delete(snap.Annotations, configvars.AnnotationMeta)
		if env != nil {
			for k, v := range env.Data {
				snap.Data[k] = append([]byte(nil), v...)
			}
			if meta := env.Annotations[configvars.AnnotationMeta]; meta != "" {
				snap.Annotations[configvars.AnnotationMeta] = meta
			}
		}
		return controllerutil.SetControllerReference(app, snap, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("snapshot release v%d: %w", n, err)
	}
	return nil
}

// pruneSnapshots deletes snapshots of releases that left the history.
func (r *AppReconciler) pruneSnapshots(ctx context.Context, app *shpyrdv1.App) {
	keep := map[string]bool{}
	for _, rel := range app.Status.Releases {
		keep[strconv.Itoa(rel.Number)] = true
	}
	var list corev1.SecretList
	if err := r.List(ctx, &list, client.InNamespace(app.Namespace), client.MatchingLabels{shpyrdv1.LabelApp: app.Name}); err != nil {
		return
	}
	for i := range list.Items {
		s := &list.Items[i]
		n, ok := s.Labels[shpyrdv1.LabelRelease]
		if !ok || keep[n] || !metav1.IsControlledBy(s, app) {
			continue
		}
		if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("could not prune release snapshot", "secret", s.Name, "err", err.Error())
		}
	}
}

// restoreRelease copies the snapshot of release n into the live config var
// Secret. It returns the restored Secret, or nil when no snapshot exists
// (releases recorded before snapshots existed): the rollback then only pins
// the build.
func (r *AppReconciler) restoreRelease(ctx context.Context, app *shpyrdv1.App, n int) (*corev1.Secret, error) {
	snap := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.ReleaseSnapshotName(n)}, snap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshot v%d: %w", n, err)
	}
	env := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: app.EnvSecretName(), Namespace: app.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, env, func() error {
		env.Type = corev1.SecretTypeOpaque
		env.Labels = mergeMaps(env.Labels, map[string]string{shpyrdv1.LabelApp: app.Name})
		env.Data = map[string][]byte{}
		for k, v := range snap.Data {
			env.Data[k] = append([]byte(nil), v...)
		}
		if env.Annotations == nil {
			env.Annotations = map[string]string{}
		}
		if meta := snap.Annotations[configvars.AnnotationMeta]; meta != "" {
			env.Annotations[configvars.AnnotationMeta] = meta
		} else {
			delete(env.Annotations, configvars.AnnotationMeta)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("restore config vars of v%d: %w", n, err)
	}
	return env, nil
}

// describeConfigChangeSince compares the live config vars with the snapshot
// of release n to name what changed.
func (r *AppReconciler) describeConfigChangeSince(ctx context.Context, app *shpyrdv1.App, n int, cur *corev1.Secret) string {
	snap := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.ReleaseSnapshotName(n)}, snap); err != nil {
		return ""
	}
	var curData map[string][]byte
	if cur != nil {
		curData = cur.Data
	}
	return describeConfigChange(snap.Data, curData)
}

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/registry"
)

// Private registries (RFC-0035 counterparts): the installer stores the
// registry credentials once, in the system namespace. Every project gets a
// mirror of that Secret and a build ServiceAccount that links it, so kpack
// and BuildKit push with it and the instances pull with it. Nothing here
// runs when the registry needs no credentials (the in-cluster registry).

// reconcileRegistryCredentials mirrors the registry Secret into the project
// namespace and keeps the build ServiceAccount in step.
func (r *AppReconciler) reconcileRegistryCredentials(ctx context.Context, app *shpyrdv1.App) error {
	if r.Config.RegistrySecret == "" {
		return nil
	}
	src := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Config.SystemNamespace, Name: r.Config.RegistrySecret}, src); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("registry credentials Secret %s/%s not found: run `shpyrd cluster init` with --registry-user and --registry-token-file", r.Config.SystemNamespace, r.Config.RegistrySecret)
		}
		return fmt.Errorf("get registry credentials: %w", err)
	}
	mirror := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: r.Config.RegistrySecret, Namespace: app.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, mirror, func() error {
		mirror.Type = src.Type
		mirror.Labels = mergeMaps(mirror.Labels, map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", shpyrdv1.LabelProject: app.Name})
		mirror.Data = src.Data
		return nil
	}); err != nil {
		return fmt.Errorf("mirror registry credentials: %w", err)
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: BuildServiceAccount, Namespace: app.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = mergeMaps(sa.Labels, map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", shpyrdv1.LabelProject: app.Name})
		sa.Secrets = []corev1.ObjectReference{{Name: r.Config.RegistrySecret}}
		sa.ImagePullSecrets = []corev1.LocalObjectReference{{Name: r.Config.RegistrySecret}}
		return nil
	}); err != nil {
		return fmt.Errorf("build service account: %w", err)
	}
	return nil
}

// registryClient returns a client for the platform registry with the
// credential of the system Secret.
func (r *AppReconciler) registryClient(ctx context.Context) (*registry.Client, error) {
	host, _, _ := registry.SplitReference(r.Config.RegistryHost + "/x")
	var data []byte
	if r.Config.RegistrySecret != "" {
		sec := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: r.Config.SystemNamespace, Name: r.Config.RegistrySecret}, sec); err != nil {
			return nil, fmt.Errorf("registry credentials: %w", err)
		}
		data = sec.Data[corev1.DockerConfigJsonKey]
	}
	return registry.FromDockerConfig(host, data), nil
}

// pruneImages deletes the images of releases that left the history from the
// in-cluster registry (RFC-0059); what cannot be deleted now stays listed
// for the next reconcile. Blobs are reclaimed by the weekly collection.
func (r *AppReconciler) pruneImages(ctx context.Context, app *shpyrdv1.App) {
	if len(app.Status.StaleImages) == 0 {
		return
	}
	if !r.Config.RegistryDeletes {
		app.Status.StaleImages = nil
		return
	}
	cl, err := r.registryClient(ctx)
	if err != nil {
		log.FromContext(ctx).Info("registry client", "err", err.Error())
		return
	}
	var remaining []string
	for _, img := range app.Status.StaleImages {
		host, repo, digest := registry.SplitReference(img)
		if host != cl.Host || digest == "" {
			continue // another registry, or no digest to delete by
		}
		if err := cl.DeleteManifest(ctx, repo, digest); err != nil {
			log.FromContext(ctx).Info("could not delete stale image", "image", img, "err", err.Error())
			remaining = append(remaining, img)
		}
	}
	app.Status.StaleImages = remaining
}

// registrySecretChanged says a Secret event is the registry credentials in
// the system namespace: every App must refresh its mirror (a rotated token
// reaches builds and pulls).
func (r *AppReconciler) registrySecretChanged(obj client.Object) bool {
	return r.Config.RegistrySecret != "" && obj.GetNamespace() == r.Config.SystemNamespace && obj.GetName() == r.Config.RegistrySecret
}

package controller

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
)

// resolvedMount is a process volume mount with its claim.
type resolvedMount struct {
	Name   string
	Path   string
	Claim  string
	Shared bool
	// Restoring says the volume is being restored from a snapshot
	// (RFC-0060): the process stops until the new claim is bound.
	Restoring bool
}

// resolveMounts validates the volumes of every process against the
// project's Volume resources: they must exist, paths must be absolute and
// unique per process, and a single-instance (ReadWriteOnce) volume can be
// mounted by one process running one instance.
func (r *AppReconciler) resolveMounts(ctx context.Context, app *shpyrdv1.App) (map[string][]resolvedMount, error) {
	out := map[string][]resolvedMount{}
	rwoUsers := map[string]string{} // volume -> process
	for _, p := range processes(app) {
		if len(p.Volumes) == 0 {
			continue
		}
		paths := map[string]bool{}
		for _, m := range p.Volumes {
			if !path.IsAbs(m.Path) || path.Clean(m.Path) == "/" {
				return nil, fmt.Errorf("process %q: volume %q needs an absolute mount path (got %q)", p.Name, m.Name, m.Path)
			}
			clean := path.Clean(m.Path)
			if paths[clean] {
				return nil, fmt.Errorf("process %q mounts two volumes at %s", p.Name, clean)
			}
			paths[clean] = true
			vol := &shpyrdv1.Volume{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: m.Name}, vol); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, fmt.Errorf("volume %q not found: create it with `shpyrd volumes create %s --size 5Gi`", m.Name, m.Name)
				}
				return nil, err
			}
			if !vol.Shared() {
				if other, ok := rwoUsers[m.Name]; ok && other != p.Name {
					return nil, fmt.Errorf("single-instance volume %q is mounted by both %s and %s: only one process can use it (make it shared to mount it from several)", m.Name, other, p.Name)
				}
				rwoUsers[m.Name] = p.Name
				if p.replicas() > 1 {
					return nil, fmt.Errorf("process %q mounts single-instance volume %q and can run 1 instance, not %d (a shared volume allows more)", p.Name, m.Name, p.replicas())
				}
			}
			out[p.Name] = append(out[p.Name], resolvedMount{Name: m.Name, Path: clean, Claim: vol.PVCName(), Shared: vol.Shared(), Restoring: vol.Status.Phase == shpyrdv1.VolumeRestoring || vol.Annotations[shpyrdv1.AnnotationRestoreFrom] != ""})
		}
	}
	return out, nil
}

// applyMounts adds the claims and mounts to a Deployment. Any
// single-instance volume forces one replica and a Recreate rollout: block
// storage attaches to one node, a rolling update would wait forever for
// the old instance to release it.
// VolumeGroup is the group volumes are handed to (the buildpack images'
// cnb group; any other image user is added to it as a supplementary group).
const VolumeGroup int64 = 1000

func applyMounts(d *appsv1.Deployment, mounts []resolvedMount) {
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Name < mounts[j].Name })
	var vols []corev1.Volume
	var vms []corev1.VolumeMount
	single, restoring := false, false
	for _, m := range mounts {
		vols = append(vols, corev1.Volume{Name: "vol-" + m.Name, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.Claim},
		}})
		vms = append(vms, corev1.VolumeMount{Name: "vol-" + m.Name, MountPath: m.Path})
		if !m.Shared {
			single = true
		}
		if m.Restoring {
			restoring = true
		}
	}
	d.Spec.Template.Spec.Volumes = vols
	if len(d.Spec.Template.Spec.Containers) > 0 {
		d.Spec.Template.Spec.Containers[0].VolumeMounts = vms
	}
	if len(vols) > 0 {
		// A freshly formatted disk belongs to root; app processes never
		// run as root (RFC-0008). fsGroup makes the kubelet hand the volume
		// to a group every container in the pod is a member of, so any
		// image user can write (only when the root differs, not a recursive
		// chown on every start). Shared (NFS) volumes are handled by their
		// export options instead; the setting is harmless there.
		if d.Spec.Template.Spec.SecurityContext == nil {
			d.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{}
		}
		d.Spec.Template.Spec.SecurityContext.FSGroup = ptr.To(VolumeGroup)
		d.Spec.Template.Spec.SecurityContext.FSGroupChangePolicy = ptr.To(corev1.FSGroupChangeOnRootMismatch)
	}
	want := appsv1.RollingUpdateDeploymentStrategyType
	if single {
		want = appsv1.RecreateDeploymentStrategyType
		d.Spec.Replicas = ptr.To[int32](1)
	}
	if restoring {
		// The claim is being replaced from a snapshot: release it.
		d.Spec.Replicas = ptr.To[int32](0)
	}
	if d.Spec.Strategy.Type != want {
		d.Spec.Strategy = appsv1.DeploymentStrategy{Type: want}
	}
}

// singleInstanceNote explains a pinned instance count in the process status.
func singleInstanceNote(mounts []resolvedMount) string {
	var names, restoring []string
	for _, m := range mounts {
		if m.Restoring {
			restoring = append(restoring, m.Name)
		}
		if !m.Shared {
			names = append(names, m.Name)
		}
	}
	if len(restoring) > 0 {
		return "stopped while volume " + strings.Join(restoring, ", ") + " is restored from a snapshot"
	}
	if len(names) == 0 {
		return ""
	}
	return "single-instance volume " + strings.Join(names, ", ")
}

// volumeToApps enqueues the Apps of the namespace that mount a Volume.
func (r *AppReconciler) volumeToApps(ctx context.Context, obj client.Object) []reconcile.Request {
	var apps shpyrdv1.AppList
	if err := r.List(ctx, &apps, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, app := range apps.Items {
		for _, p := range app.Spec.Processes {
			for _, m := range p.Volumes {
				if m.Name == obj.GetName() {
					reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&app)})
				}
			}
		}
	}
	return reqs
}

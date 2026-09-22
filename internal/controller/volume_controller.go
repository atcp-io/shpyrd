package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// LabelVolume marks the claim of a Volume.
const LabelVolume = "shpyrd.io/volume"

// VolumeReconciler turns Volume resources into PersistentVolumeClaims
// (RFC-0006). The claim is owned by the Volume, never by a workload, so
// deploys and scaling cannot touch the data.
type VolumeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&shpyrdv1.Volume{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&shpyrdv1.App{}, handler.EnqueueRequestsFromMapFunc(appToVolumes)).
		Complete(r)
}

// appToVolumes enqueues the volumes an App mounts so mountedBy stays current.
func appToVolumes(_ context.Context, obj client.Object) []reconcile.Request {
	app, ok := obj.(*shpyrdv1.App)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var reqs []reconcile.Request
	for _, p := range app.Spec.Processes {
		for _, v := range p.Volumes {
			if !seen[v.Name] {
				seen[v.Name] = true
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: v.Name}})
			}
		}
	}
	return reqs
}

func (r *VolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	vol := &shpyrdv1.Volume{}
	if err := r.Get(ctx, req.NamespacedName, vol); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !vol.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := vol.DeepCopy()

	err := r.reconcile(ctx, vol)
	if apierrors.IsConflict(err) {
		return ctrl.Result{Requeue: true}, nil
	}
	if err != nil {
		logger.Error(err, "reconcile volume failed")
		vol.Status.Phase = shpyrdv1.VolumeFailed
		vol.Status.Message = err.Error()
		setVolumeCondition(vol, metav1.ConditionFalse, "Error", err.Error())
		r.Recorder.Event(vol, corev1.EventTypeWarning, "ReconcileError", err.Error())
	}
	vol.Status.ObservedGeneration = vol.Generation
	if statusErr := r.Status().Patch(ctx, vol, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); statusErr != nil {
		if apierrors.IsConflict(statusErr) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, statusErr
	}
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *VolumeReconciler) reconcile(ctx context.Context, vol *shpyrdv1.Volume) error {
	if vol.Spec.Size.Sign() <= 0 {
		return fmt.Errorf("size must be positive")
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: vol.Namespace, Name: vol.PVCName()}, pvc)
	switch {
	case apierrors.IsNotFound(err):
		pvc = desiredPVC(vol)
		if err := controllerutil.SetControllerReference(vol, pvc, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, pvc); err != nil {
			return fmt.Errorf("create claim: %w", err)
		}
		r.Recorder.Eventf(vol, corev1.EventTypeNormal, "Provisioning", "created claim %s (%s, %s)", pvc.Name, vol.Spec.Size.String(), vol.Mode())
	case err != nil:
		return fmt.Errorf("get claim: %w", err)
	default:
		if err := r.reconcileExisting(ctx, vol, pvc); err != nil {
			return err
		}
	}

	mountedBy, err := r.mountedBy(ctx, vol)
	if err != nil {
		return err
	}
	vol.Status.MountedBy = mountedBy
	if cap, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		vol.Status.Capacity = cap.String()
	}
	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		vol.Status.Phase = shpyrdv1.VolumeBound
		vol.Status.Message = ""
		if req := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; vol.Status.Capacity != "" && req.Cmp(vol.Spec.Size) == 0 {
			if cap := pvc.Status.Capacity[corev1.ResourceStorage]; cap.Cmp(vol.Spec.Size) < 0 {
				vol.Status.Message = fmt.Sprintf("expanding to %s", vol.Spec.Size.String())
			}
		}
		setVolumeCondition(vol, metav1.ConditionTrue, "Bound", "claim bound")
	case corev1.ClaimLost:
		vol.Status.Phase = shpyrdv1.VolumeFailed
		vol.Status.Message = "the underlying volume was lost"
		setVolumeCondition(vol, metav1.ConditionFalse, "Lost", vol.Status.Message)
	default:
		vol.Status.Phase = shpyrdv1.VolumePending
		if len(mountedBy) == 0 {
			vol.Status.Message = "created; the disk is provisioned when a process mounts it"
		} else {
			vol.Status.Message = "provisioning"
		}
		setVolumeCondition(vol, metav1.ConditionFalse, "Pending", vol.Status.Message)
	}
	return nil
}

// reconcileExisting applies allowed changes (growth) and refuses the rest.
func (r *VolumeReconciler) reconcileExisting(ctx context.Context, vol *shpyrdv1.Volume, pvc *corev1.PersistentVolumeClaim) error {
	if len(pvc.Spec.AccessModes) > 0 && pvc.Spec.AccessModes[0] != vol.Mode() {
		return fmt.Errorf("access mode cannot change after creation (claim is %s): delete the volume and create it again", pvc.Spec.AccessModes[0])
	}
	if vol.Spec.StorageClass != "" && pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != vol.Spec.StorageClass {
		return fmt.Errorf("storage class cannot change after creation (claim uses %s)", *pvc.Spec.StorageClassName)
	}
	current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	switch cmp := vol.Spec.Size.Cmp(current); {
	case cmp < 0:
		return fmt.Errorf("volumes cannot shrink (currently %s)", current.String())
	case cmp > 0:
		ok, className, err := r.expansionAllowed(ctx, pvc)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("storage class %q does not allow volume expansion: create a new volume and copy the data", className)
		}
		patch := client.MergeFrom(pvc.DeepCopy())
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = vol.Spec.Size
		if err := r.Patch(ctx, pvc, patch); err != nil {
			return fmt.Errorf("expand claim: %w", err)
		}
		r.Recorder.Eventf(vol, corev1.EventTypeNormal, "Expanding", "claim %s grows from %s to %s", pvc.Name, current.String(), vol.Spec.Size.String())
	}
	return nil
}

// expansionAllowed checks the claim's storage class (or the default one).
func (r *VolumeReconciler) expansionAllowed(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (bool, string, error) {
	var classes storagev1.StorageClassList
	if err := r.List(ctx, &classes); err != nil {
		return false, "", fmt.Errorf("list storage classes: %w", err)
	}
	name := ""
	if pvc.Spec.StorageClassName != nil {
		name = *pvc.Spec.StorageClassName
	}
	for _, sc := range classes.Items {
		isDefault := sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true"
		if sc.Name == name || (name == "" && isDefault) {
			return sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion, sc.Name, nil
		}
	}
	return false, name, nil
}

// mountedBy lists "<app>/<process>" pairs mounting the volume.
func (r *VolumeReconciler) mountedBy(ctx context.Context, vol *shpyrdv1.Volume) ([]string, error) {
	var apps shpyrdv1.AppList
	if err := r.List(ctx, &apps, client.InNamespace(vol.Namespace)); err != nil {
		return nil, err
	}
	var out []string
	for _, app := range apps.Items {
		for name, p := range app.Spec.Processes {
			for _, m := range p.Volumes {
				if m.Name == vol.Name {
					out = append(out, app.Name+"/"+name)
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func desiredPVC(vol *shpyrdv1.Volume) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vol.PVCName(),
			Namespace: vol.Namespace,
			Labels:    map[string]string{LabelVolume: vol.Name, shpyrdv1.LabelManagedBy: "shpyrd"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{vol.Mode()},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: vol.Spec.Size}},
		},
	}
	if vol.Spec.StorageClass != "" {
		pvc.Spec.StorageClassName = &vol.Spec.StorageClass
	}
	return pvc
}

func setVolumeCondition(vol *shpyrdv1.Volume, status metav1.ConditionStatus, reason, msg string) {
	cond := metav1.Condition{Type: shpyrdv1.ConditionReady, Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.Now(), ObservedGeneration: vol.Generation}
	for i, c := range vol.Status.Conditions {
		if c.Type == cond.Type {
			if c.Status == cond.Status {
				cond.LastTransitionTime = c.LastTransitionTime
			}
			vol.Status.Conditions[i] = cond
			return
		}
	}
	vol.Status.Conditions = append(vol.Status.Conditions, cond)
}

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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
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
	// Profile storage (RFC-0060): the classes claims use when the Volume
	// names none (single-instance and shared), and the VolumeSnapshotClass
	// snapshots are taken with ("" when the cluster has none).
	DefaultClass  string
	SharedClass   string
	SnapshotClass string
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
	// A finished in-place restore: drop the request now that the status
	// says so (RFC-0060).
	if snap := vol.Annotations[shpyrdv1.AnnotationRestoreFrom]; snap != "" && vol.Status.Phase != shpyrdv1.VolumeRestoring && vol.Status.RestoredFrom == snap {
		r.clearRestore(ctx, vol)
	}
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if vol.Status.Phase == shpyrdv1.VolumeRestoring {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *VolumeReconciler) reconcile(ctx context.Context, vol *shpyrdv1.Volume) error {
	if vol.Spec.Size.Sign() <= 0 {
		return fmt.Errorf("size must be positive")
	}
	if snap := vol.Annotations[shpyrdv1.AnnotationRestoreFrom]; snap != "" {
		return r.restoreInPlace(ctx, vol, snap)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: vol.Namespace, Name: vol.PVCName()}, pvc)
	switch {
	case apierrors.IsNotFound(err):
		pvc = r.desiredPVC(vol, vol.Spec.FromSnapshot)
		if err := r.checkStorageClass(ctx, vol, pvc); err != nil {
			return err
		}
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
	if pvc.Spec.StorageClassName != nil {
		vol.Status.StorageClass = *pvc.Spec.StorageClassName
	}
	if pvc.Spec.DataSource != nil && pvc.Spec.DataSource.Kind == "VolumeSnapshot" {
		vol.Status.RestoredFrom = pvc.Spec.DataSource.Name
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

func (r *VolumeReconciler) desiredPVC(vol *shpyrdv1.Volume, fromSnapshot string) *corev1.PersistentVolumeClaim {
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
	if class := r.storageClassFor(vol); class != "" {
		pvc.Spec.StorageClassName = &class
	}
	if fromSnapshot != "" {
		pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{
			APIGroup: ptr.To("snapshot.storage.k8s.io"),
			Kind:     "VolumeSnapshot",
			Name:     fromSnapshot,
		}
	}
	return pvc
}

// checkStorageClass refuses a claim whose class does not exist, with a
// message that says what is missing; a claim on an unknown class would sit
// Pending with the reason in an event nobody reads.
func (r *VolumeReconciler) checkStorageClass(ctx context.Context, vol *shpyrdv1.Volume, pvc *corev1.PersistentVolumeClaim) error {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return nil
	}
	class := *pvc.Spec.StorageClassName
	sc := &storagev1.StorageClass{}
	err := r.Get(ctx, types.NamespacedName{Name: class}, sc)
	switch {
	case apierrors.IsNotFound(err) && vol.Spec.StorageClass == "" && vol.Shared():
		return fmt.Errorf("shared volumes are not set up on this cluster (storage class %s does not exist): on Oracle Cloud create the File Storage mount target with contrib/oci/terraform and pass --set SHPYRD_FSS_MOUNT_TARGET and --set SHPYRD_FSS_AD to `shpyrd cluster init`", class)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("storage class %s does not exist on this cluster (`kubectl get storageclass` lists the available ones)", class)
	case err != nil:
		return fmt.Errorf("get storage class %s: %w", class, err)
	}
	return nil
}

// storageClassFor is the class a Volume's claim uses: the one it names, else
// the profile's default for its access mode ("" leaves the cluster default).
func (r *VolumeReconciler) storageClassFor(vol *shpyrdv1.Volume) string {
	if vol.Spec.StorageClass != "" {
		return vol.Spec.StorageClass
	}
	if vol.Shared() {
		return r.SharedClass
	}
	return r.DefaultClass
}

// VolumeSnapshotGVK is the CSI snapshot API.
var VolumeSnapshotGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}

// restoreInPlace replaces the volume's claim with one restored from a
// snapshot (RFC-0060): the volume reports Restoring, which makes the App
// controller stop the processes mounting it; once no pod uses the claim it
// is deleted and recreated from the snapshot; when the new claim is bound
// the annotation goes and the processes come back.
func (r *VolumeReconciler) restoreInPlace(ctx context.Context, vol *shpyrdv1.Volume, snapshot string) error {
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(VolumeSnapshotGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: vol.Namespace, Name: snapshot}, snap); err != nil {
		if apierrors.IsNotFound(err) {
			r.clearRestore(ctx, vol)
			return fmt.Errorf("snapshot %q not found; restore cancelled", snapshot)
		}
		return err
	}
	if ready, _, _ := unstructured.NestedBool(snap.Object, "status", "readyToUse"); !ready {
		vol.Status.Phase = shpyrdv1.VolumeRestoring
		vol.Status.Message = fmt.Sprintf("waiting for snapshot %s to be ready", snapshot)
		return nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: vol.Namespace, Name: vol.PVCName()}, pvc)
	switch {
	case apierrors.IsNotFound(err):
		// The old claim is gone: create the new one from the snapshot.
		pvc = r.desiredPVC(vol, snapshot)
		if id := vol.Annotations[shpyrdv1.AnnotationRestoreID]; id != "" {
			pvc.Annotations = map[string]string{shpyrdv1.AnnotationRestoreID: id}
		}
		if err := controllerutil.SetControllerReference(vol, pvc, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, pvc); err != nil {
			return fmt.Errorf("create claim from snapshot: %w", err)
		}
		r.Recorder.Eventf(vol, corev1.EventTypeNormal, "Restoring", "claim %s recreated from snapshot %s", pvc.Name, snapshot)
		vol.Status.Phase = shpyrdv1.VolumeRestoring
		vol.Status.Message = "restoring from snapshot " + snapshot
		return nil
	case err != nil:
		return fmt.Errorf("get claim: %w", err)
	}
	// The claim of this request: same snapshot and the same request id (a
	// claim restored from that snapshot earlier is old data by now).
	restoredFromThis := pvc.Spec.DataSource != nil && pvc.Spec.DataSource.Kind == "VolumeSnapshot" && pvc.Spec.DataSource.Name == snapshot &&
		pvc.Annotations[shpyrdv1.AnnotationRestoreID] == vol.Annotations[shpyrdv1.AnnotationRestoreID]
	if restoredFromThis {
		// The new claim carries the snapshot: the restore is done from the
		// volume's point of view. Whether the disk is created now or when
		// the process mounts it is the storage class's binding mode (OCI
		// binds on first consumer), so the processes must come back for it
		// to bind; Reconcile removes the request once the status is written
		// (a metadata patch here would race the status patch).
		vol.Status.RestoredFrom = snapshot
		if pvc.Status.Phase == corev1.ClaimBound {
			vol.Status.Phase = shpyrdv1.VolumeBound
			vol.Status.Message = ""
			if cap, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
				vol.Status.Capacity = cap.String()
			}
			setVolumeCondition(vol, metav1.ConditionTrue, "Bound", "restored from snapshot "+snapshot)
		} else {
			vol.Status.Phase = shpyrdv1.VolumePending
			vol.Status.Message = "restored from snapshot " + snapshot + "; the disk is provisioned when a process mounts it"
			setVolumeCondition(vol, metav1.ConditionFalse, "Pending", vol.Status.Message)
		}
		r.Recorder.Eventf(vol, corev1.EventTypeNormal, "Restored", "volume restored from snapshot %s", snapshot)
		return nil
	}
	// The old claim: wait for its users to stop, then delete it.
	vol.Status.Phase = shpyrdv1.VolumeRestoring
	users, err := r.podsUsing(ctx, vol.Namespace, pvc.Name)
	if err != nil {
		return err
	}
	if users > 0 {
		vol.Status.Message = fmt.Sprintf("stopping %d instance(s) that mount the volume", users)
		return nil
	}
	if pvc.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete claim: %w", err)
		}
	}
	vol.Status.Message = "replacing the volume with snapshot " + snapshot
	return nil
}

// clearRestore removes the restore request from the Volume.
func (r *VolumeReconciler) clearRestore(ctx context.Context, vol *shpyrdv1.Volume) {
	patch := client.MergeFrom(vol.DeepCopy())
	delete(vol.Annotations, shpyrdv1.AnnotationRestoreFrom)
	delete(vol.Annotations, shpyrdv1.AnnotationRestoreID)
	if err := r.Patch(ctx, vol, patch); err != nil {
		log.FromContext(ctx).Info("could not clear the restore annotation", "err", err.Error())
	}
}

// podsUsing counts non-terminated pods mounting the claim.
func (r *VolumeReconciler) podsUsing(ctx context.Context, namespace, claim string) (int, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
				n++
				break
			}
		}
	}
	return n, nil
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

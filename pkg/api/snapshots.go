package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/internal/controller"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kexec"
)

// SnapshotView is a volume snapshot as shown to users (RFC-0060).
type SnapshotView struct {
	Name      string    `json:"name"`
	Volume    string    `json:"volume"`
	Size      string    `json:"size,omitempty"`
	Ready     bool      `json:"ready"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// CreateSnapshotRequest takes a snapshot of a volume.
type CreateSnapshotRequest struct {
	// Name of the snapshot; defaults to <volume>-<timestamp>.
	Name string `json:"name,omitempty"`
}

// RestoreVolumeRequest restores a volume from one of its snapshots: into a
// new volume when To is set, otherwise in place.
type RestoreVolumeRequest struct {
	Snapshot string `json:"snapshot" binding:"required"`
	To       string `json:"to,omitempty"`
}

// RestoreVolumeResponse reports what the restore did.
type RestoreVolumeResponse struct {
	Volume  VolumeView `json:"volume"`
	InPlace bool       `json:"inPlace"`
	Message string     `json:"message"`
}

// ErrSnapshotsUnavailable is returned where the profile has no snapshot
// class (local clusters, providers without CSI snapshots).
var ErrSnapshotsUnavailable = errors.New("volume snapshots are not available on this cluster: its profile has no snapshot class (cloud profiles such as oci provide one; SHPYRD_SNAPSHOT_CLASS)")

func (s *Server) snapshotClass(c *gin.Context) (string, bool) {
	class := s.vars(install.VarSnapshotClass)
	if class == "" {
		abort(c, http.StatusNotImplemented, ErrSnapshotsUnavailable)
		return "", false
	}
	return class, true
}

func snapshotView(u unstructured.Unstructured) SnapshotView {
	out := SnapshotView{Name: u.GetName(), Volume: u.GetLabels()[shpyrdv1.LabelVolumeOf], CreatedAt: u.GetCreationTimestamp().Time}
	if out.Volume == "" {
		out.Volume, _, _ = unstructured.NestedString(u.Object, "spec", "source", "persistentVolumeClaimName")
	}
	out.Ready, _, _ = unstructured.NestedBool(u.Object, "status", "readyToUse")
	if size, ok, _ := unstructured.NestedString(u.Object, "status", "restoreSize"); ok {
		out.Size = size
	}
	if msg, ok, _ := unstructured.NestedString(u.Object, "status", "error", "message"); ok && msg != "" {
		out.Message = msg
	} else if !out.Ready {
		out.Message = "taking snapshot"
	}
	return out
}

func (s *Server) getVolume(c *gin.Context) (*shpyrdv1.Volume, bool) {
	key := types.NamespacedName{Namespace: projectNamespace(c), Name: c.Param("name")}
	vol := &shpyrdv1.Volume{}
	if err := s.apps.Get(c.Request.Context(), key, vol); err != nil {
		abortNotFound(c, err, "volume")
		return nil, false
	}
	return vol, true
}

// listSnapshots lists the snapshots taken from a volume, newest first.
func (s *Server) listSnapshots(c *gin.Context) {
	if _, ok := s.snapshotClass(c); !ok {
		return
	}
	vol, ok := s.getVolume(c)
	if !ok {
		return
	}
	items, err := s.volumeSnapshots(c.Request.Context(), vol)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, items)
}

func (s *Server) volumeSnapshots(ctx context.Context, vol *shpyrdv1.Volume) ([]SnapshotView, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(controller.VolumeSnapshotGVK.GroupVersion().WithKind("VolumeSnapshotList"))
	if err := s.apps.List(ctx, list, client.InNamespace(vol.Namespace), client.MatchingLabels{shpyrdv1.LabelVolumeOf: vol.Name}); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, ErrSnapshotsUnavailable
		}
		return nil, err
	}
	out := make([]SnapshotView, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, snapshotView(item))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// createSnapshot takes a snapshot of the volume's claim.
func (s *Server) createSnapshot(c *gin.Context) {
	class, ok := s.snapshotClass(c)
	if !ok {
		return
	}
	var req CreateSnapshotRequest
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
	}
	vol, ok := s.getVolume(c)
	if !ok {
		return
	}
	if vol.Status.Phase != shpyrdv1.VolumeBound {
		abort(c, http.StatusConflict, fmt.Errorf("volume %q has no disk to snapshot yet (%s): mount it once first", vol.Name, firstNonEmpty(vol.Status.Phase, shpyrdv1.VolumePending)))
		return
	}
	name := req.Name
	if name == "" {
		name = fmt.Sprintf("%s-%s", vol.Name, time.Now().UTC().Format("20060102-1504"))
	}
	if !volumeName.MatchString(name) || len(name) > 63 {
		abort(c, http.StatusBadRequest, errors.New("snapshot name must be lowercase letters, digits and dashes"))
		return
	}
	// Block snapshots are crash-consistent: they hold what reached the
	// disk. Ask the instances mounting the volume to flush first, so what
	// the application wrote a moment ago is in the copy (best effort: an
	// image without `sync` is skipped).
	s.syncClaim(c.Request.Context(), vol.Namespace, vol.PVCName())
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(controller.VolumeSnapshotGVK)
	snap.SetName(name)
	snap.SetNamespace(vol.Namespace)
	snap.SetLabels(map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", shpyrdv1.LabelVolumeOf: vol.Name})
	_ = unstructured.SetNestedField(snap.Object, class, "spec", "volumeSnapshotClassName")
	_ = unstructured.SetNestedField(snap.Object, vol.PVCName(), "spec", "source", "persistentVolumeClaimName")
	if err := s.apps.Create(c.Request.Context(), snap); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			abort(c, http.StatusConflict, fmt.Errorf("snapshot %q already exists", name))
		case meta.IsNoMatchError(err):
			abort(c, http.StatusNotImplemented, ErrSnapshotsUnavailable)
		default:
			abort(c, http.StatusBadGateway, err)
		}
		return
	}
	s.audit(c, c.Param("slug"), "volume.snapshot", vol.Name, name)
	c.JSON(http.StatusCreated, snapshotView(*snap))
}

// syncClaim runs `sync` in every running pod that mounts the claim.
func (s *Server) syncClaim(ctx context.Context, namespace, claim string) {
	if s.kube == nil || s.kube.Kube == nil || s.kube.Config == nil {
		return
	}
	pods, err := s.kube.Kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != claim {
				continue
			}
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if _, err := kexec.Run(sctx, s.kube, namespace, p.Name, "", []string{"sync"}); err != nil {
				s.log.Info("snapshot: sync in instance skipped", "pod", p.Name, "err", err.Error())
			}
			cancel()
			break
		}
	}
}

// deleteSnapshot removes a snapshot (and the provider's copy).
func (s *Server) deleteSnapshot(c *gin.Context) {
	if _, ok := s.snapshotClass(c); !ok {
		return
	}
	vol, ok := s.getVolume(c)
	if !ok {
		return
	}
	snap, err := s.projectSnapshot(c.Request.Context(), vol.Namespace, c.Param("snap"))
	if err != nil {
		abortNotFound(c, err, "snapshot")
		return
	}
	if owner := snap.GetLabels()[shpyrdv1.LabelVolumeOf]; owner != "" && owner != vol.Name {
		abort(c, http.StatusNotFound, fmt.Errorf("snapshot %q belongs to volume %q", snap.GetName(), owner))
		return
	}
	if err := s.apps.Delete(c.Request.Context(), snap); client.IgnoreNotFound(err) != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	s.audit(c, c.Param("slug"), "volume.snapshot.delete", vol.Name, snap.GetName())
	c.Status(http.StatusNoContent)
}

func (s *Server) projectSnapshot(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(controller.VolumeSnapshotGVK)
	if err := s.apps.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, snap); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, ErrSnapshotsUnavailable
		}
		return nil, err
	}
	return snap, nil
}

// checkSnapshotUsable verifies a snapshot exists in the project and grows
// the requested size to the snapshot's if smaller (a claim cannot be
// restored into less space).
func (s *Server) checkSnapshotUsable(ctx context.Context, namespace, name string, size *resource.Quantity) error {
	if s.vars(install.VarSnapshotClass) == "" {
		return ErrSnapshotsUnavailable
	}
	snap, err := s.projectSnapshot(ctx, namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("snapshot %q not found in this project", name)
		}
		return err
	}
	if restoreSize, ok, _ := unstructured.NestedString(snap.Object, "status", "restoreSize"); ok && restoreSize != "" {
		if q, err := resource.ParseQuantity(restoreSize); err == nil && size.Cmp(q) < 0 {
			*size = q
		}
	}
	return nil
}

// restoreVolume restores a snapshot into a new volume (to) or in place.
// In place, the processes mounting the volume are stopped while the claim
// is replaced; no release is created either way.
func (s *Server) restoreVolume(c *gin.Context) {
	if _, ok := s.snapshotClass(c); !ok {
		return
	}
	var req RestoreVolumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	vol, ok := s.getVolume(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	snap, err := s.projectSnapshot(ctx, vol.Namespace, req.Snapshot)
	if err != nil {
		abortNotFound(c, err, "snapshot")
		return
	}
	if owner := snap.GetLabels()[shpyrdv1.LabelVolumeOf]; owner != "" && owner != vol.Name {
		abort(c, http.StatusBadRequest, fmt.Errorf("snapshot %q was taken from volume %q; restore it there or into a new volume from that one", snap.GetName(), owner))
		return
	}
	if ready, _, _ := unstructured.NestedBool(snap.Object, "status", "readyToUse"); !ready {
		abort(c, http.StatusConflict, fmt.Errorf("snapshot %q is not ready yet", snap.GetName()))
		return
	}
	if req.To != "" {
		if !volumeName.MatchString(req.To) {
			abort(c, http.StatusBadRequest, errors.New("new volume name must be lowercase letters, digits and dashes (max 40 chars)"))
			return
		}
		size := vol.Spec.Size.DeepCopy()
		if err := s.checkSnapshotUsable(ctx, vol.Namespace, snap.GetName(), &size); err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
		copyVol := &shpyrdv1.Volume{
			ObjectMeta: metav1.ObjectMeta{Name: req.To, Namespace: vol.Namespace, Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}},
			Spec:       shpyrdv1.VolumeSpec{Size: size, StorageClass: vol.Spec.StorageClass, AccessMode: vol.Spec.AccessMode, FromSnapshot: snap.GetName()},
		}
		if err := s.apps.Create(ctx, copyVol); err != nil {
			if apierrors.IsAlreadyExists(err) {
				abort(c, http.StatusConflict, fmt.Errorf("volume %q already exists", req.To))
			} else {
				abort(c, http.StatusBadGateway, err)
			}
			return
		}
		s.audit(c, c.Param("slug"), "volume.restore", vol.Name, fmt.Sprintf("%s -> %s", snap.GetName(), req.To))
		c.JSON(http.StatusCreated, RestoreVolumeResponse{Volume: volumeView(*copyVol), Message: fmt.Sprintf("volume %s is being created from snapshot %s; mount it to use the data", req.To, snap.GetName())})
		return
	}
	if vol.Status.Phase == shpyrdv1.VolumeRestoring {
		abort(c, http.StatusConflict, fmt.Errorf("volume %q is already being restored", vol.Name))
		return
	}
	patch := client.MergeFrom(vol.DeepCopy())
	if vol.Annotations == nil {
		vol.Annotations = map[string]string{}
	}
	vol.Annotations[shpyrdv1.AnnotationRestoreFrom] = snap.GetName()
	vol.Annotations[shpyrdv1.AnnotationRestoreID] = strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := s.apps.Patch(ctx, vol, patch); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	s.audit(c, c.Param("slug"), "volume.restore", vol.Name, snap.GetName())
	msg := fmt.Sprintf("restoring %s from snapshot %s in place", vol.Name, snap.GetName())
	if len(vol.Status.MountedBy) > 0 {
		msg += fmt.Sprintf("; %v stop while the disk is replaced and start again when it is ready", vol.Status.MountedBy)
	}
	c.JSON(http.StatusAccepted, RestoreVolumeResponse{Volume: volumeView(*vol), InPlace: true, Message: msg})
}

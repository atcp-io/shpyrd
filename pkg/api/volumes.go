package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/install"
)

// VolumeView is a project volume as shown to users.
type VolumeView struct {
	Name         string    `json:"name"`
	Namespace    string    `json:"namespace"`
	Size         string    `json:"size"`
	Capacity     string    `json:"capacity,omitempty"`
	Shared       bool      `json:"shared"`
	StorageClass string    `json:"storageClass,omitempty"`
	Phase        string    `json:"phase"`
	Message      string    `json:"message,omitempty"`
	MountedBy    []string  `json:"mountedBy"`
	CreatedAt    time.Time `json:"createdAt"`
	// RestoredFrom is the snapshot the volume was restored from (RFC-0060).
	RestoredFrom string `json:"restoredFrom,omitempty"`
	// Note explains a provider rule applied at creation, such as a size
	// rounded up to the provider's minimum (RFC-0060).
	Note string `json:"note,omitempty"`
}

func volumeView(v shpyrdv1.Volume) VolumeView {
	out := VolumeView{
		Name: v.Name, Namespace: v.Namespace, Size: v.Spec.Size.String(), Capacity: v.Status.Capacity,
		Shared: v.Shared(), StorageClass: firstNonEmpty(v.Status.StorageClass, v.Spec.StorageClass), Phase: firstNonEmpty(v.Status.Phase, shpyrdv1.VolumePending),
		Message: v.Status.Message, MountedBy: v.Status.MountedBy, CreatedAt: v.CreationTimestamp.Time,
		RestoredFrom: v.Status.RestoredFrom,
	}
	if out.MountedBy == nil {
		out.MountedBy = []string{}
	}
	return out
}

var volumeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)

// CreateVolumeRequest creates a Volume in a project namespace.
type CreateVolumeRequest struct {
	Name         string `json:"name" binding:"required"`
	Size         string `json:"size" binding:"required"`
	StorageClass string `json:"storageClass,omitempty"`
	Shared       bool   `json:"shared,omitempty"`
	// FromSnapshot starts the volume from a snapshot of the project
	// (RFC-0060) instead of empty.
	FromSnapshot string `json:"fromSnapshot,omitempty"`
}

// ResizeVolumeRequest grows a volume.
type ResizeVolumeRequest struct {
	Size string `json:"size" binding:"required"`
}

func (s *Server) listVolumes(c *gin.Context) {
	var list shpyrdv1.VolumeList
	if err := s.apps.List(c.Request.Context(), &list, client.InNamespace(s.projectNamespace(c))); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]VolumeView, 0, len(list.Items))
	for _, v := range list.Items {
		out = append(out, volumeView(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	c.JSON(http.StatusOK, out)
}

func (s *Server) createVolume(c *gin.Context) {
	var req CreateVolumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if !volumeName.MatchString(req.Name) {
		abort(c, http.StatusBadRequest, errors.New("name must be lowercase letters, digits and dashes (max 40 chars)"))
		return
	}
	size, err := parseSize(req.Size)
	if err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	// Provider minimum (RFC-0060): the disk would be that size anyway; say
	// so before it exists instead of after the bill. Shared volumes are
	// file systems, which have no such minimum and ignore the size.
	var note string
	if req.Shared {
		if s.vars(install.VarFSSMountTarget) != "" {
			note = "shared volumes are Oracle Cloud File Storage file systems: the size is not enforced, the volume grows as needed and billing follows use"
		}
	} else {
		size, note = s.applyVolumeMinimum(size)
	}
	vol := &shpyrdv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: s.projectNamespace(c), Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", shpyrdv1.LabelWorkspace: s.workspace(c), shpyrdv1.LabelProject: c.Param("slug")}},
		Spec:       shpyrdv1.VolumeSpec{Size: size, StorageClass: req.StorageClass, AccessMode: corev1.ReadWriteOnce, FromSnapshot: req.FromSnapshot},
	}
	if req.Shared {
		vol.Spec.AccessMode = corev1.ReadWriteMany
	}
	if req.FromSnapshot != "" {
		if err := s.checkSnapshotUsable(c.Request.Context(), vol.Namespace, req.FromSnapshot, &vol.Spec.Size); err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
	}
	if err := s.checkVolumeClass(c.Request.Context(), vol); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if err := s.checkPlanStorage(c.Request.Context(), s.workspace(c), vol.Spec.Size); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if err := s.apps.Create(c.Request.Context(), vol); err != nil {
		if apierrors.IsAlreadyExists(err) {
			abort(c, http.StatusConflict, fmt.Errorf("volume %q already exists", req.Name))
		} else {
			abort(c, http.StatusBadGateway, err)
		}
		return
	}
	s.audit(c, c.Param("slug"), "volume.create", vol.Name, vol.Spec.Size.String())
	view := volumeView(*vol)
	view.Note = note
	c.JSON(http.StatusCreated, view)
}

// applyVolumeMinimum rounds a request up to the profile's minimum volume
// size and explains it.
func (s *Server) applyVolumeMinimum(size resource.Quantity) (resource.Quantity, string) {
	minStr := s.vars(install.VarVolumeMinSize)
	if minStr == "" {
		return size, ""
	}
	minimum, err := resource.ParseQuantity(minStr)
	if err != nil || size.Cmp(minimum) >= 0 {
		return size, ""
	}
	provider := "volumes on this cluster"
	if s.vars(install.VarProfile) == "oci" {
		provider = "Oracle Cloud block volumes"
	}
	return minimum, fmt.Sprintf("%s start at %s: created at %s instead of %s", provider, minimum.String(), minimum.String(), size.String())
}

// checkVolumeClass refuses a volume whose storage class does not exist on
// the cluster before anything is created: the shared class in particular
// is installed only where the profile's shared storage is set up.
func (s *Server) checkVolumeClass(ctx context.Context, vol *shpyrdv1.Volume) error {
	class := vol.Spec.StorageClass
	if class == "" {
		if vol.Shared() {
			class = s.vars(install.VarStorageClassShared)
		} else {
			class = s.vars(install.VarStorageClass)
		}
	}
	if class == "" || s.kube == nil || s.kube.Kube == nil {
		return nil
	}
	_, err := s.kube.Kube.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err) && vol.Spec.StorageClass == "" && vol.Shared():
		hint := "no ReadWriteMany storage class is installed"
		if s.vars(install.VarProfile) == "oci" {
			hint = "create the File Storage mount target with contrib/oci/terraform (shared_storage = true) and pass --set SHPYRD_FSS_MOUNT_TARGET and --set SHPYRD_FSS_AD to `shpyrd cluster init`"
		}
		return fmt.Errorf("shared volumes are not set up on this cluster (storage class %s does not exist): %s", class, hint)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("storage class %q does not exist on this cluster", class)
	case err != nil:
		return fmt.Errorf("check storage class %s: %w", class, err)
	}
	return nil
}

func (s *Server) resizeVolume(c *gin.Context) {
	var req ResizeVolumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	size, err := parseSize(req.Size)
	if err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	key := types.NamespacedName{Namespace: s.projectNamespace(c), Name: c.Param("name")}
	vol := &shpyrdv1.Volume{}
	if err := s.apps.Get(c.Request.Context(), key, vol); err != nil {
		abortNotFound(c, err, "volume")
		return
	}
	if floor := volumeFloor(vol); size.Cmp(floor) < 0 {
		abort(c, http.StatusBadRequest, fmt.Errorf("volumes cannot shrink (currently %s)", floor.String()))
		return
	}
	if !vol.Shared() {
		if rounded, note := s.applyVolumeMinimum(size); note != "" {
			size = rounded
		}
	}
	growth := size.DeepCopy()
	growth.Sub(vol.Spec.Size)
	if err := s.checkPlanStorage(c.Request.Context(), s.workspace(c), growth); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	vol.Spec.Size = size
	if err := s.apps.Update(c.Request.Context(), vol); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	s.audit(c, c.Param("slug"), "volume.resize", vol.Name, req.Size)
	c.JSON(http.StatusOK, volumeView(*vol))
}

// deleteVolume removes a volume and its data. Mounted volumes are refused
// unless ?force=true.
func (s *Server) deleteVolume(c *gin.Context) {
	key := types.NamespacedName{Namespace: s.projectNamespace(c), Name: c.Param("name")}
	vol := &shpyrdv1.Volume{}
	if err := s.apps.Get(c.Request.Context(), key, vol); err != nil {
		abortNotFound(c, err, "volume")
		return
	}
	if len(vol.Status.MountedBy) > 0 && c.Query("force") != "true" {
		abort(c, http.StatusConflict, fmt.Errorf("volume %q is mounted by %v: unmount it first (or force)", vol.Name, vol.Status.MountedBy))
		return
	}
	if err := s.apps.Delete(c.Request.Context(), vol); client.IgnoreNotFound(err) != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	s.audit(c, c.Param("slug"), "volume.delete", vol.Name, "")
	c.Status(http.StatusNoContent)
}

// volumeFloor is the smallest size a volume may be set to: what the claim
// actually has. Setting the spec back to it recovers from a refused expansion.
func volumeFloor(vol *shpyrdv1.Volume) resource.Quantity {
	if vol.Status.Capacity != "" {
		if q, err := resource.ParseQuantity(vol.Status.Capacity); err == nil {
			return q
		}
	}
	return vol.Spec.Size
}

func parseSize(s string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return q, fmt.Errorf("invalid size %q (use e.g. 5Gi)", s)
	}
	if q.Sign() <= 0 {
		return q, errors.New("size must be positive")
	}
	return q, nil
}

func abortNotFound(c *gin.Context, err error, what string) {
	if apierrors.IsNotFound(err) {
		abort(c, http.StatusNotFound, errors.New(what+" not found"))
		return
	}
	abort(c, http.StatusBadGateway, err)
}

// singleInstanceVolume returns the name of a ReadWriteOnce volume mounted
// by the process, or "" when it may scale.
func (s *Server) singleInstanceVolume(ctx context.Context, app *shpyrdv1.App, process string) string {
	p, ok := app.Spec.Processes[process]
	if !ok {
		return ""
	}
	for _, m := range p.Volumes {
		vol := &shpyrdv1.Volume{}
		if err := s.apps.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: m.Name}, vol); err != nil {
			continue
		}
		if !vol.Shared() {
			return m.Name
		}
	}
	return ""
}

// checkScale refuses instance counts a single-instance volume forbids.
func (s *Server) checkScale(ctx context.Context, app *shpyrdv1.App, process string, replicas int32) error {
	if replicas <= 1 {
		return nil
	}
	if v := s.singleInstanceVolume(ctx, app, process); v != "" {
		return fmt.Errorf("%s mounts single-instance volume %q and can run 1 instance (a shared volume allows more)", process, v)
	}
	return nil
}

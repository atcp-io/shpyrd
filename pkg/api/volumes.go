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
	"shpyrd/pkg/authz"
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
}

func volumeView(v shpyrdv1.Volume) VolumeView {
	out := VolumeView{
		Name: v.Name, Namespace: v.Namespace, Size: v.Spec.Size.String(), Capacity: v.Status.Capacity,
		Shared: v.Shared(), StorageClass: v.Spec.StorageClass, Phase: firstNonEmpty(v.Status.Phase, shpyrdv1.VolumePending),
		Message: v.Status.Message, MountedBy: v.Status.MountedBy, CreatedAt: v.CreationTimestamp.Time,
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
}

// ResizeVolumeRequest grows a volume.
type ResizeVolumeRequest struct {
	Size string `json:"size" binding:"required"`
}

func (s *Server) listVolumes(c *gin.Context) {
	var list shpyrdv1.VolumeList
	if err := s.apps.List(c.Request.Context(), &list, client.InNamespace(c.Param("ns"))); err != nil {
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
	vol := &shpyrdv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: c.Param("ns"), Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}},
		Spec:       shpyrdv1.VolumeSpec{Size: size, StorageClass: req.StorageClass, AccessMode: corev1.ReadWriteOnce},
	}
	if req.Shared {
		vol.Spec.AccessMode = corev1.ReadWriteMany
	}
	if err := s.apps.Create(c.Request.Context(), vol); err != nil {
		if apierrors.IsAlreadyExists(err) {
			abort(c, http.StatusConflict, fmt.Errorf("volume %q already exists", req.Name))
		} else {
			abort(c, http.StatusBadGateway, err)
		}
		return
	}
	s.audit(c, authz.ProjectFromNamespace(vol.Namespace), "volume.create", vol.Name, req.Size)
	c.JSON(http.StatusCreated, volumeView(*vol))
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
	key := types.NamespacedName{Namespace: c.Param("ns"), Name: c.Param("name")}
	vol := &shpyrdv1.Volume{}
	if err := s.apps.Get(c.Request.Context(), key, vol); err != nil {
		abortNotFound(c, err, "volume")
		return
	}
	if floor := volumeFloor(vol); size.Cmp(floor) < 0 {
		abort(c, http.StatusBadRequest, fmt.Errorf("volumes cannot shrink (currently %s)", floor.String()))
		return
	}
	vol.Spec.Size = size
	if err := s.apps.Update(c.Request.Context(), vol); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	s.audit(c, authz.ProjectFromNamespace(vol.Namespace), "volume.resize", vol.Name, req.Size)
	c.JSON(http.StatusOK, volumeView(*vol))
}

// deleteVolume removes a volume and its data. Mounted volumes are refused
// unless ?force=true.
func (s *Server) deleteVolume(c *gin.Context) {
	key := types.NamespacedName{Namespace: c.Param("ns"), Name: c.Param("name")}
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
	s.audit(c, authz.ProjectFromNamespace(vol.Namespace), "volume.delete", vol.Name, "")
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

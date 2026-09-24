package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Volume phases.
const (
	VolumePending = "Pending"
	VolumeBound   = "Bound"
	VolumeFailed  = "Failed"
	// VolumeRestoring: the claim is being replaced by one restored from a
	// snapshot (RFC-0060); processes mounting the volume are stopped.
	VolumeRestoring = "Restoring"

	// AnnotationRestoreFrom, on a Volume, names the snapshot to restore in
	// place (RFC-0060). The controller removes it when done.
	AnnotationRestoreFrom = "shpyrd.io/restore-from"
	// AnnotationRestoreID, next to AnnotationRestoreFrom, identifies one
	// restore request; the controller stamps it on the claim it recreates
	// so a later restore from the same snapshot is not mistaken for done.
	AnnotationRestoreID = "shpyrd.io/restore-id"
	// LabelVolumeOf marks a VolumeSnapshot with the Volume it was taken from.
	LabelVolumeOf = "shpyrd.io/volume"
)

// PVCPrefix names the PersistentVolumeClaim of a Volume (vol-<name>).
const PVCPrefix = "vol-"

// VolumeSpec describes a persistent disk of a project (RFC-0006).
type VolumeSpec struct {
	// Size of the volume, e.g. 5Gi. Can grow (never shrink) when the
	// storage class allows expansion.
	Size resource.Quantity `json:"size"`
	// StorageClass to provision from; empty means the cluster default.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
	// AccessMode is ReadWriteOnce (default: a single-instance volume, the
	// mounting process runs one instance with Recreate rollouts) or
	// ReadWriteMany (shared: needs a provisioner that offers it).
	// +optional
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
	// FromSnapshot creates the volume from a snapshot (a VolumeSnapshot in
	// the project) instead of empty (RFC-0060). Immutable.
	// +optional
	FromSnapshot string `json:"fromSnapshot,omitempty"`
}

// VolumeStatus reports the state of the claim.
type VolumeStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is Pending, Bound or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Capacity actually provisioned.
	// +optional
	Capacity string `json:"capacity,omitempty"`
	// MountedBy lists "<app>/<process>" pairs using the volume.
	// +optional
	MountedBy []string `json:"mountedBy,omitempty"`
	// StorageClass the claim was provisioned from (the profile's default
	// when the spec names none).
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
	// RestoredFrom is the snapshot the current claim was restored from, if
	// any (RFC-0060).
	// +optional
	RestoredFrom string `json:"restoredFrom,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vol
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.accessMode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Volume is a persistent disk that processes of the project mount.
type Volume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeSpec   `json:"spec"`
	Status VolumeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeList contains a list of Volume.
type VolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Volume `json:"items"`
}

// PVCName is the claim backing the volume.
func (v *Volume) PVCName() string { return PVCPrefix + v.Name }

// Mode returns the effective access mode.
func (v *Volume) Mode() corev1.PersistentVolumeAccessMode {
	if v.Spec.AccessMode == "" {
		return corev1.ReadWriteOnce
	}
	return v.Spec.AccessMode
}

// Shared reports whether several instances may mount the volume at once.
func (v *Volume) Shared() bool { return v.Mode() == corev1.ReadWriteMany }

func init() {
	SchemeBuilder.Register(&Volume{}, &VolumeList{})
}

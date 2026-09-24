package controller

import (
	"k8s.io/apimachinery/pkg/api/resource"
)

// StorageProfile is what a cloud profile says about disks (RFC-0060): the
// class claims use when nothing names one, and the provider minimum a
// request is rounded up to. Datastores (Postgres, Redis) apply it to their
// data volumes; project volumes apply it through the VolumeReconciler.
type StorageProfile struct {
	// Class for single-instance (ReadWriteOnce) claims; "" leaves the
	// cluster default.
	Class string
	// MinSize as a quantity string, e.g. "50Gi"; "" for none.
	MinSize string
}

// Size rounds a request up to the minimum and reports whether it did.
func (p StorageProfile) Size(requested resource.Quantity) (resource.Quantity, bool) {
	if p.MinSize == "" {
		return requested, false
	}
	minimum, err := resource.ParseQuantity(p.MinSize)
	if err != nil || requested.Cmp(minimum) >= 0 {
		return requested, false
	}
	return minimum, true
}

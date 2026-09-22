// Package v1alpha1 contains the shpyrd.io/v1alpha1 API: the App resource.
//
// +kubebuilder:object:generate=true
// +groupName=shpyrd.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version of this API.
	GroupVersion = schema.GroupVersion{Group: "shpyrd.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

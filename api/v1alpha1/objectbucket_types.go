package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectBucket phases.
const (
	BucketPending = "Pending"
	BucketReady   = "Ready"
	BucketFailed  = "Failed"
)

// ObjectBucketSpec asks the platform's object store (RFC-0046) for a bucket
// with a credential that opens it and nothing else. The bucket is named
// after the resource and its namespace; the credential lands in a Secret
// next to the resource.
type ObjectBucketSpec struct {
	// SecretName of the credential Secret written in the resource's
	// namespace (default: <name>-object-storage).
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// RetentionDays expires objects older than this (0: keep everything).
	// +optional
	// +kubebuilder:validation:Minimum=0
	RetentionDays int32 `json:"retentionDays,omitempty"`
	// Versioning keeps previous versions of overwritten objects.
	// +optional
	Versioning bool `json:"versioning,omitempty"`
	// DeletionPolicy says what happens to the bucket's contents when the
	// resource is deleted: Delete (default) removes the bucket and its
	// objects, Retain leaves them for an operator to remove.
	// +optional
	// +kubebuilder:validation:Enum=Delete;Retain
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// ObjectBucketStatus reports the bucket and its usage.
type ObjectBucketStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is Pending, Ready or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Bucket is the bucket's name in the store.
	// +optional
	Bucket string `json:"bucket,omitempty"`
	// Endpoint is the S3 endpoint URL consumers use.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// SecretName holds the credential (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY,
	// AWS_ENDPOINT_URL, BUCKET, AWS_REGION; accessKeyId and secretAccessKey).
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// UsedBytes and Objects are the last measured usage.
	// +optional
	UsedBytes int64 `json:"usedBytes,omitempty"`
	// +optional
	Objects int64 `json:"objects,omitempty"`
	// +optional
	MeasuredAt *metav1.Time `json:"measuredAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bucket
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.status.bucket`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Used",type=integer,JSONPath=`.status.usedBytes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ObjectBucket is a bucket in the platform's object store with its own
// credential (RFC-0046).
type ObjectBucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectBucketSpec   `json:"spec,omitempty"`
	Status ObjectBucketStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ObjectBucketList contains a list of ObjectBucket.
type ObjectBucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectBucket `json:"items"`
}

// BucketName is the bucket's name in the store: shpyrd-<namespace>-<name>,
// which is unique across the cluster and valid for S3 (lowercase, dashes).
func (b *ObjectBucket) BucketName() string { return "shpyrd-" + b.Namespace + "-" + b.Name }

// CredentialSecretName is the Secret the credential is written to.
func (b *ObjectBucket) CredentialSecretName() string {
	if b.Spec.SecretName != "" {
		return b.Spec.SecretName
	}
	return b.Name + "-object-storage"
}

func init() {
	SchemeBuilder.Register(&ObjectBucket{}, &ObjectBucketList{})
}

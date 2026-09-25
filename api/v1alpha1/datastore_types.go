package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Resource phases shared by data stores (RFC-0003): Pending, Provisioning,
// Ready, Failed.
const (
	ResourcePending      = "Pending"
	ResourceProvisioning = "Provisioning"
	ResourceReady        = "Ready"
	ResourceFailed       = "Failed"
)

// ResourceStatus is the status shape every project resource shares so the
// dashboard renders any of them.
type ResourceStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is Pending, Provisioning, Ready or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Endpoint is host:port inside the project.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// CredentialsSecret holds the connection details the binding exposes.
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
	// Storage is the effective data volume size when it differs from the
	// request: the provider's minimum applied (RFC-0060).
	// +optional
	Storage string `json:"storage,omitempty"`
	// LastBackup is when the last successful base backup finished; RecoverableFrom
	// is the earliest point in time the backups can restore to (RFC-0038).
	// +optional
	LastBackup *metav1.Time `json:"lastBackup,omitempty"`
	// +optional
	RecoverableFrom *metav1.Time `json:"recoverableFrom,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ---- Postgres (RFC-0009) ----------------------------------------------------

// PostgresSpec describes a PostgreSQL database run by CloudNativePG.
type PostgresSpec struct {
	// Version is the PostgreSQL major version (default "17").
	// +optional
	Version string `json:"version,omitempty"`
	// Size is an instance size from the cluster catalog (default: catalog default).
	// +optional
	Size string `json:"size,omitempty"`
	// Storage is the data volume size (default 5Gi); it can grow.
	// +optional
	Storage *resource.Quantity `json:"storage,omitempty"`
	// Instances is the number of PostgreSQL instances (1, or 2-3 for HA).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	Instances *int32 `json:"instances,omitempty"`
	// Backups turns on continuous WAL archiving and scheduled base backups
	// to the platform's object store (RFC-0038); needs the object-storage
	// extension.
	// +optional
	Backups *PostgresBackups `json:"backups,omitempty"`
	// Recovery bootstraps this database from another one's backups, at a
	// point in time (RFC-0038); the source keeps running.
	// +optional
	Recovery *PostgresRecovery `json:"recovery,omitempty"`
}

// PostgresBackups configures backups of a database.
type PostgresBackups struct {
	// Schedule of base backups, five-field cron in UTC (default "0 2 * * *",
	// every day at 02:00).
	// +optional
	Schedule string `json:"schedule,omitempty"`
	// Retention of backups and WAL, as <n>d (default 14d).
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+d$`
	Retention string `json:"retention,omitempty"`
}

// PostgresRecovery names the source and the moment to recover to.
type PostgresRecovery struct {
	// From is the Postgres resource in the same project whose backups are
	// restored.
	From string `json:"from"`
	// TargetTime is the point in time to recover to; the latest possible
	// when unset.
	// +optional
	TargetTime *metav1.Time `json:"targetTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pg
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Postgres is a PostgreSQL database of a project.
type Postgres struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresSpec   `json:"spec"`
	Status ResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PostgresList contains a list of Postgres.
type PostgresList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Postgres `json:"items"`
}

// ---- Redis (RFC-0010) -------------------------------------------------------

// RedisSpec describes a Redis-compatible store (Valkey by default).
type RedisSpec struct {
	// Engine is "valkey" (default) or "redis".
	// +optional
	// +kubebuilder:validation:Enum=valkey;redis
	Engine string `json:"engine,omitempty"`
	// Version is the engine's major version (default: valkey 8, redis 7).
	// +optional
	Version string `json:"version,omitempty"`
	// Size is an instance size from the cluster catalog.
	// +optional
	Size string `json:"size,omitempty"`
	// Persistent keeps data on a volume (AOF); false is a pure cache that
	// loses its content on restart.
	// +optional
	Persistent bool `json:"persistent,omitempty"`
	// Storage is the volume size when persistent (default 1Gi).
	// +optional
	Storage *resource.Quantity `json:"storage,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="Persistent",type=boolean,JSONPath=`.spec.persistent`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`

// Redis is a Redis-compatible store of a project.
type Redis struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RedisSpec      `json:"spec"`
	Status ResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RedisList contains a list of Redis.
type RedisList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Redis `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Postgres{}, &PostgresList{}, &Redis{}, &RedisList{})
}

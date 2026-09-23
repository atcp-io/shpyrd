package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LogDrain phases.
const (
	DrainPending = "Pending"
	DrainActive  = "Active"
	DrainFailing = "Failing"
)

// Drain formats.
const (
	DrainFormatJSON   = "json"
	DrainFormatSyslog = "syslog"
)

// LogDrainSpec forwards log lines to an external receiver (RFC-0023). A
// drain in a project namespace (app-<slug>) receives that project's lines;
// a drain in the system namespace receives every project's lines.
type LogDrainSpec struct {
	// URL of the receiver: https:// or http:// for json, syslog:// or
	// syslog+tls:// (host:port) for syslog.
	URL string `json:"url"`
	// Format is json (one JSON object per line, HTTP batches) or syslog
	// (RFC 5424 over TCP). Defaults from the URL scheme.
	// +optional
	// +kubebuilder:validation:Enum=json;syslog
	Format string `json:"format,omitempty"`
	// HeadersFrom names a Secret in the same namespace whose keys are HTTP
	// header names and values their values (API keys). Never shown again.
	// +optional
	HeadersFrom *corev1.LocalObjectReference `json:"headersFrom,omitempty"`
	// Processes limits the drain to these process types; empty means all.
	// +optional
	Processes []string `json:"processes,omitempty"`
}

// LogDrainStatus reports delivery.
type LogDrainStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is Pending (nothing sent yet), Active or Failing.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// LastDeliveryAt is when lines last reached the receiver.
	// +optional
	LastDeliveryAt *metav1.Time `json:"lastDeliveryAt,omitempty"`
	// Sent counts events delivered since the agent started.
	// +optional
	Sent int64 `json:"sent,omitempty"`
	// Errors counts delivery errors since the agent started.
	// +optional
	Errors int64 `json:"errors,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=drain
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Format",type=string,JSONPath=`.spec.format`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// LogDrain forwards a project's (or every project's) log lines to a receiver.
type LogDrain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LogDrainSpec   `json:"spec"`
	Status LogDrainStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LogDrainList contains a list of LogDrain.
type LogDrainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogDrain `json:"items"`
}

// EffectiveFormat is the format in effect: the spec's, or derived from the URL.
func (d *LogDrain) EffectiveFormat() string {
	if d.Spec.Format != "" {
		return d.Spec.Format
	}
	if len(d.Spec.URL) >= 6 && d.Spec.URL[:6] == "syslog" {
		return DrainFormatSyslog
	}
	return DrainFormatJSON
}

func init() {
	SchemeBuilder.Register(&LogDrain{}, &LogDrainList{})
}

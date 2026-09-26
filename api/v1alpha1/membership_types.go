package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Roles (RFC-0008). Project roles are granted per project; platform roles
// apply to the whole cluster.
const (
	// RoleUser opens the app (the edge, RFC-0033) and nothing in the
	// builder dashboard beyond the launcher.
	RoleUser      = "user"
	RoleViewer    = "viewer"
	RoleDeveloper = "developer"
	RoleAdmin     = "admin"

	RolePlatformAdmin  = "platform-admin"
	RolePlatformViewer = "platform-viewer"
)

// TeamSpec groups users. Members are matched by email; Groups are names of
// the identity provider's groups claim, so a company directory group maps
// to a team without listing people twice.
type TeamSpec struct {
	// +optional
	Description string `json:"description,omitempty"`
	// Members are user emails.
	// +optional
	Members []string `json:"members,omitempty"`
	// Groups are identity provider group names whose users belong to the team.
	// +optional
	Groups []string `json:"groups,omitempty"`
	// PlatformRole grants a cluster-wide role to the team: platform-admin
	// or platform-viewer.
	// +optional
	// +kubebuilder:validation:Enum=platform-admin;platform-viewer
	PlatformRole string `json:"platformRole,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Members",type=string,JSONPath=`.spec.members`
// +kubebuilder:printcolumn:name="Platform role",type=string,JSONPath=`.spec.platformRole`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Team is a group of users; projects grant roles to teams or to users.
type Team struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TeamSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// TeamList contains a list of Team.
type TeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Team `json:"items"`
}

// ProjectMemberSpec grants a project role to a user or a team.
type ProjectMemberSpec struct {
	// Project name (the namespace is app-<project>).
	Project string `json:"project"`
	// Role is viewer, developer or admin.
	// +kubebuilder:validation:Enum=viewer;developer;admin
	Role string `json:"role"`
	// User email; exactly one of User and Team is set.
	// +optional
	User string `json:"user,omitempty"`
	// Team name.
	// +optional
	Team string `json:"team,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=member
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.project`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="User",type=string,JSONPath=`.spec.user`
// +kubebuilder:printcolumn:name="Team",type=string,JSONPath=`.spec.team`

// ProjectMember grants a role on a project to a user or a team.
type ProjectMember struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProjectMemberSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ProjectMemberList contains a list of ProjectMember.
type ProjectMemberList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProjectMember `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Team{}, &TeamList{}, &ProjectMember{}, &ProjectMemberList{})
}

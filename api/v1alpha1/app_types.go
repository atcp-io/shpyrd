package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Well known labels and annotations.
const (
	// LabelApp marks every object that belongs to an App (value: app name).
	LabelApp = "shpyrd.io/app"
	// LabelProcess marks workloads with their process type (web, worker...).
	LabelProcess = "shpyrd.io/process"
	// LabelManagedBy is set on namespaces created for apps.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// AnnotationConfigHash is put on pod templates so config changes roll out.
	AnnotationConfigHash = "shpyrd.io/config-hash"
	// AnnotationReleaseNote lets clients describe the next release
	// (for example "Rollback to v3"); the controller consumes it.
	AnnotationReleaseNote = "shpyrd.io/release-note"
	// AnnotationRollbackTo asks the controller to restore the config vars
	// snapshot of that release number before reconciling; consumed once.
	AnnotationRollbackTo = "shpyrd.io/rollback-to"
	// LabelRelease marks per-release snapshots (config var Secrets).
	LabelRelease = "shpyrd.io/release"
	// DefaultWebPort is the port web processes listen on ($PORT).
	DefaultWebPort int32 = 8080
	// EnvSecretSuffix: the Secret <app>-env holds config vars set with
	// `shpyrd secrets`.
	EnvSecretSuffix = "-env"
)

// Phases of an App.
const (
	PhasePending   = "Pending"   // no source or image yet
	PhaseBuilding  = "Building"  // kpack build in progress
	PhaseDeploying = "Deploying" // workloads rolling out
	PhaseRunning   = "Running"
	PhaseFailed    = "Failed"
)

// Condition types.
const (
	ConditionReady = "Ready"
	ConditionBuilt = "Built"
)

// AppSpec is the desired state of an application.
type AppSpec struct {
	// Source is where the application code comes from. It is built into an
	// image with buildpacks. Leave empty when Image is set.
	// +optional
	Source *Source `json:"source,omitempty"`

	// Image runs a prebuilt image instead of building Source. Set by
	// `shpyrd deploy --local-build` and by rollbacks; cleared by the next
	// source deploy.
	// +optional
	Image string `json:"image,omitempty"`

	// Build tunes the buildpacks build.
	// +optional
	Build *Build `json:"build,omitempty"`

	// Processes maps process types (web, worker, ...) to their settings.
	// Defaults to a single "web" process.
	// +optional
	Processes map[string]Process `json:"processes,omitempty"`

	// Env are plain environment variables for every process. Secrets are
	// managed separately with `shpyrd secrets` and live in Secret <app>-env.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Domains served by the web process. Defaults to <name>.<cluster domain>.
	// +optional
	Domains []string `json:"domains,omitempty"`
}

// Source describes the application source code.
type Source struct {
	// +optional
	Git *GitSource `json:"git,omitempty"`
	// +optional
	Blob *BlobSource `json:"blob,omitempty"`
	// SubPath is a directory inside the source that holds the application.
	// +optional
	SubPath string `json:"subPath,omitempty"`
}

// GitSource builds from a Git repository; kpack polls branches for changes.
type GitSource struct {
	URL string `json:"url"`
	// Revision is a branch, tag or commit. Defaults to the default branch
	// as resolved by kpack ("main" when unset).
	// +optional
	Revision string `json:"revision,omitempty"`
}

// BlobSource builds from an archive uploaded with `shpyrd deploy`.
type BlobSource struct {
	// URL is reachable from inside the cluster (served by shpyrd-server).
	URL string `json:"url"`
	// SHA256 of the archive; identifies the release source.
	// +optional
	SHA256 string `json:"sha256,omitempty"`
	// Ref is a human readable origin such as the git commit.
	// +optional
	Ref string `json:"ref,omitempty"`
}

// Build tunes the buildpacks build.
type Build struct {
	// Env are build-time variables (BP_*).
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Builder is the kpack ClusterBuilder to use. Defaults to "shpyrd".
	// +optional
	Builder string `json:"builder,omitempty"`
}

// Process is one process type of the app.
type Process struct {
	// Replicas defaults to 1.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`
	// Port the process listens on; exposed through a Service and, for
	// "web", through the Ingress. Defaults to 8080 for web, none otherwise.
	// +optional
	Port *int32 `json:"port,omitempty"`
	// Command overrides the buildpacks launcher (/cnb/process/<type>).
	// +optional
	Command []string `json:"command,omitempty"`
	// Args are appended to the command.
	// +optional
	Args []string `json:"args,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// AppStatus is the observed state of an App.
type AppStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is a coarse summary: Pending, Building, Deploying, Running, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Message explains the phase (build failure, rollout progress...).
	// +optional
	Message string `json:"message,omitempty"`
	// Image currently deployed (digest reference).
	// +optional
	Image string `json:"image,omitempty"`
	// URL of the web process, when exposed.
	// +optional
	URL string `json:"url,omitempty"`
	// LatestBuild is the kpack Build producing (or having produced) Image.
	// +optional
	LatestBuild string `json:"latestBuild,omitempty"`
	// Releases is the deployment history, oldest first.
	// +optional
	Releases []Release `json:"releases,omitempty"`
	// Processes reports rollout state per process type.
	// +optional
	Processes map[string]ProcessStatus `json:"processes,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Release is one entry of the deployment history.
type Release struct {
	Number int    `json:"number"`
	Image  string `json:"image"`
	// Source identifies the code (git commit or archive sha).
	// +optional
	Source string `json:"source,omitempty"`
	// ConfigHash covers env vars and the <app>-env Secret.
	// +optional
	ConfigHash  string      `json:"configHash,omitempty"`
	Description string      `json:"description,omitempty"`
	CreatedAt   metav1.Time `json:"createdAt"`
	// Processes lists the process types the release ran with; rolling back
	// to a build that lacks a current process type cannot start it.
	// +optional
	Processes []string `json:"processes,omitempty"`
}

// ProcessStatus is the rollout state of one process type.
type ProcessStatus struct {
	Desired int32 `json:"desired"`
	Ready   int32 `json:"ready"`
	// +optional
	Updated int32 `json:"updated,omitempty"`
	// Failing counts instances that cannot start or keep crashing.
	// +optional
	Failing int32 `json:"failing,omitempty"`
	// Reason explains Failing (e.g. "CrashLoopBackOff (exit 128): ...").
	// +optional
	Reason string `json:"reason,omitempty"`
}

// App is an application managed by shpyrd. It lives in the namespace that
// holds its workloads (app-<name> by default).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=app,categories=shpyrd
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Releases",type=integer,JSONPath=`.status.releases[-1:].number`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type App struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppSpec   `json:"spec,omitempty"`
	Status AppStatus `json:"status,omitempty"`
}

// AppList is a list of App.
//
// +kubebuilder:object:root=true
type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []App `json:"items"`
}

func init() {
	SchemeBuilder.Register(&App{}, &AppList{})
}

// EnvSecretName is the Secret holding the app's config vars.
func (a *App) EnvSecretName() string { return a.Name + EnvSecretSuffix }

// ReleaseSnapshotName is the Secret holding the config vars as they were
// when release n was created; rollback restores it.
func (a *App) ReleaseSnapshotName(n int) string { return fmt.Sprintf("%s-release-v%d", a.Name, n) }

// HasSource reports whether a build source is configured.
func (a *App) HasSource() bool {
	return a.Spec.Source != nil && (a.Spec.Source.Git != nil || a.Spec.Source.Blob != nil)
}

// CurrentRelease returns the latest release or nil.
func (a *App) CurrentRelease() *Release {
	if len(a.Status.Releases) == 0 {
		return nil
	}
	return &a.Status.Releases[len(a.Status.Releases)-1]
}

// ReleaseByNumber finds a release in the history.
func (a *App) ReleaseByNumber(n int) *Release {
	for i := range a.Status.Releases {
		if a.Status.Releases[i].Number == n {
			return &a.Status.Releases[i]
		}
	}
	return nil
}

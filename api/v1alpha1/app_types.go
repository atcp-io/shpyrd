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
	// LabelProject marks the namespace of a project (value: project slug).
	LabelProject = "shpyrd.io/project"
	// AnnotationDisplayName on an App holds the human name of the project
	// when it differs from the slug (RFC-0011).
	AnnotationDisplayName = "shpyrd.io/display-name"
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
	// AnnotationBindingProviders on the <app>-bindings Secret maps each
	// variable to the "<Kind> <name>" resource providing it (JSON).
	AnnotationBindingProviders = "shpyrd.io/binding-providers"
	// LabelBuildNumber is set on Dockerfile build Jobs (1, 2, 3...).
	LabelBuildNumber = "shpyrd.io/build-number"
	// LabelBuild is set on build pods with the name of their build.
	LabelBuild = "shpyrd.io/build"
	// AnnotationBuildKey identifies what a build Job built (source and
	// build settings); a different key means a new build is needed.
	AnnotationBuildKey = "shpyrd.io/build-key"
	// AnnotationBuildImage holds the pushed digest reference of a
	// finished build Job.
	AnnotationBuildImage = "shpyrd.io/build-image"
	// AnnotationBuildRevision holds the git commit a build Job resolved.
	AnnotationBuildRevision = "shpyrd.io/build-revision"
	// AnnotationBuildFailure holds the failure summary of a build Job.
	AnnotationBuildFailure = "shpyrd.io/build-failure"
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

	// Bindings attach project resources (Postgres, Redis, ...) to the app:
	// their connection details become config vars of every process.
	// +optional
	Bindings []Binding `json:"bindings,omitempty"`
	// Globals controls the cluster-wide config vars a platform admin sets
	// with `shpyrd globals` (RFC-0016). Nil injects all of them.
	// +optional
	Globals *Globals `json:"globals,omitempty"`
}

// Globals is a project's opt-out from cluster-wide config vars.
type Globals struct {
	// Disabled leaves every global var out of this project.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// Exclude names global vars this project does not receive.
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// Binding attaches a project resource to an app (RFC-0003). The resource's
// connection details are injected as config vars named <prefix>_<VAR>,
// e.g. DATABASE_URL, through the Secret <app>-bindings.
type Binding struct {
	// Kind of the resource (Postgres, Redis, ...).
	Kind string `json:"kind"`
	// Name of the resource in the project.
	Name string `json:"name"`
	// Prefix of the injected variable names; defaults per kind.
	// +optional
	Prefix string `json:"prefix,omitempty"`
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

// VolumeMount mounts a project Volume into a process.
type VolumeMount struct {
	// Name of the Volume resource.
	Name string `json:"name"`
	// Path inside the container.
	Path string `json:"path"`
}

// Build strategies.
const (
	// StrategyBuildpacks builds with Cloud Native Buildpacks through kpack.
	StrategyBuildpacks = "buildpacks"
	// StrategyDockerfile builds the source's Dockerfile with BuildKit.
	StrategyDockerfile = "dockerfile"
)

// Build tunes how the source is turned into an image.
type Build struct {
	// Strategy is "buildpacks" (default) or "dockerfile".
	// +optional
	// +kubebuilder:validation:Enum=buildpacks;dockerfile
	Strategy string `json:"strategy,omitempty"`
	// Env are build-time variables: BP_* for buildpacks, build args for
	// Dockerfiles.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Builder is the kpack ClusterBuilder to use. Defaults to "shpyrd".
	// +optional
	Builder string `json:"builder,omitempty"`
	// Dockerfile is the path of the Dockerfile inside the source (after
	// subPath). Defaults to "Dockerfile".
	// +optional
	Dockerfile string `json:"dockerfile,omitempty"`
	// Target is the multi-stage build target.
	// +optional
	Target string `json:"target,omitempty"`
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
	// Required for process types other than web with Dockerfile builds,
	// whose images have a single entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`
	// Args are appended to the command.
	// +optional
	Args []string `json:"args,omitempty"`
	// Size names an instance size from the cluster catalog (shared-s,
	// dedicated-m, ...). Empty means the catalog default.
	// +optional
	Size string `json:"size,omitempty"`
	// HealthCheck configures the readiness, liveness and startup probes.
	// Defaults to HTTP GET / on the port (web), TCP on the port (processes
	// with an explicit port), or nothing (workers without a port). RFC-0019.
	// +optional
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`
	// Volumes mounts Volume resources of the project. A ReadWriteOnce
	// volume pins the process to one instance with Recreate rollouts.
	// +optional
	Volumes []VolumeMount `json:"volumes,omitempty"`
	// Resources override the size (cpu/memory limits); rarely needed.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// HealthCheck describes the probe a process uses to report readiness.
// Exactly one of Path, TCP or Command should be set; if none are set the
// default for the process type is used (see Process.HealthCheck).
type HealthCheck struct {
	// Path is the HTTP path the process answers on (GET on PORT).
	// +optional
	Path string `json:"path,omitempty"`
	// TCP checks the port; no body is read.
	// +optional
	TCP bool `json:"tcp,omitempty"`
	// Command runs inside the container; exit 0 = healthy.
	// +optional
	Command []string `json:"command,omitempty"`
	// Interval between checks (default 10s).
	// +optional
	// +kubebuilder:validation:Pattern=`^\d+(s|m)$`
	Interval string `json:"interval,omitempty"`
	// Timeout per check (default 5s).
	// +optional
	// +kubebuilder:validation:Pattern=`^\d+(s|m)$`
	Timeout string `json:"timeout,omitempty"`
	// GracePeriod is how long a newly started instance has before probe
	// failures count against it (startup probe; default 30s).
	// +optional
	// +kubebuilder:validation:Pattern=`^\d+(s|m)$`
	GracePeriod string `json:"gracePeriod,omitempty"`
	// ShutdownDelay keeps the instance in the rotation after SIGTERM is
	// announced so the load balancer has time to drain (default 5s).
	// +optional
	// +kubebuilder:validation:Pattern=`^\d+(s|m)$`
	ShutdownDelay string `json:"shutdownDelay,omitempty"`
	// Disabled turns off all probes for the process; the deploy does not
	// wait for readiness.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
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
	// ConfigHash covers env vars, the <app>-env Secret, bound vars, sizes
	// and the global vars injected.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`
	// GlobalHash fingerprints the global vars injected at the time, so a
	// release can be told apart as "Global config change" (RFC-0016).
	// +optional
	GlobalHash  string      `json:"globalHash,omitempty"`
	Description string      `json:"description,omitempty"`
	CreatedAt   metav1.Time `json:"createdAt"`
	// Processes lists the process types the release ran with; rolling back
	// to a build that lacks a current process type cannot start it.
	// +optional
	Processes []string `json:"processes,omitempty"`
	// Sizes records the instance size of each process type; rollback
	// restores them.
	// +optional
	Sizes map[string]string `json:"sizes,omitempty"`
	// Bindings records the resources attached to the release; rollback
	// restores them.
	// +optional
	Bindings []Binding `json:"bindings,omitempty"`
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
	// Size is the instance size in effect ("custom" when overridden).
	// +optional
	Size string `json:"size,omitempty"`
	// CPU and Memory are the allocation in effect (requests), for display.
	// +optional
	CPU string `json:"cpu,omitempty"`
	// +optional
	Memory string `json:"memory,omitempty"`
	// Pinned explains a fixed instance count ("single-instance volume data").
	// +optional
	Pinned string `json:"pinned,omitempty"`
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

// GlobalEnvSecretName is the cluster-wide config vars Secret in the system
// namespace and the name of its filtered mirror in every project namespace
// (RFC-0016).
const GlobalEnvSecretName = "shpyrd-global-env"

// BindingsSecretSuffix: the Secret <app>-bindings holds the config vars
// provided by attached resources; the controller owns it.
const BindingsSecretSuffix = "-bindings"

// BindingsSecretName is the Secret with the config vars of attached resources.
func (a *App) BindingsSecretName() string { return a.Name + BindingsSecretSuffix }

// ReleaseSnapshotName is the Secret holding the config vars as they were
// when release n was created; rollback restores it.
func (a *App) ReleaseSnapshotName(n int) string { return fmt.Sprintf("%s-release-v%d", a.Name, n) }

// HasSource reports whether a build source is configured.
func (a *App) HasSource() bool {
	return a.Spec.Source != nil && (a.Spec.Source.Git != nil || a.Spec.Source.Blob != nil)
}

// BuildStrategy returns the effective build strategy.
func (a *App) BuildStrategy() string {
	if a.Spec.Build != nil && a.Spec.Build.Strategy == StrategyDockerfile {
		return StrategyDockerfile
	}
	return StrategyBuildpacks
}

// UsesBuildpacks reports whether the running image was produced by
// buildpacks, i.e. has the CNB launcher and /cnb/process/<type> entries.
func (a *App) UsesBuildpacks() bool {
	return a.HasSource() && a.BuildStrategy() == StrategyBuildpacks
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

// getStr is a nil-safe getter for HealthCheck string fields by name.
// GetStr returns a HealthCheck string field by name; nil-safe.
func (h *HealthCheck) GetStr(field string) string {
	if h == nil {
		return ""
	}
	switch field {
	case "Interval":
		return h.Interval
	case "Timeout":
		return h.Timeout
	case "GracePeriod":
		return h.GracePeriod
	case "ShutdownDelay":
		return h.ShutdownDelay
	}
	return ""
}

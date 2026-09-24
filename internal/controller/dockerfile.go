package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/buildtrust"
)

// Dockerfile builds run as Kubernetes Jobs: an init container fetches the
// source (uploaded archive or git clone) into an emptyDir, then rootless
// BuildKit builds it with a local context and pushes to the registry. The
// pushed digest is written to the container's termination message; on
// failure Kubernetes puts the log tail there instead
// (FallbackToLogsOnError), which is what the user sees as the reason.

const (
	buildContainer = "build"
	fetchContainer = "fetch"
	workspaceDir   = "/workspace"

	// History kept per app, like kpack's build history limits.
	successfulBuildsKept = 10
	failedBuildsKept     = 5
)

// buildResult is what the build container reports on success.
type buildResult struct {
	Image    string `json:"image"`
	Revision string `json:"revision,omitempty"`
}

// reconcileDockerfileBuild makes sure a Job exists for the current source and
// build settings and reports its state.
func (r *AppReconciler) reconcileDockerfileBuild(ctx context.Context, app *shpyrdv1.App) (buildState, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(app.Namespace), client.MatchingLabels{shpyrdv1.LabelApp: app.Name}); err != nil {
		return buildState{}, fmt.Errorf("list builds: %w", err)
	}
	builds := buildJobs(jobs.Items)
	key := buildKey(app)

	var latest *batchv1.Job
	if len(builds) > 0 {
		latest = &builds[len(builds)-1]
	}
	if latest == nil || latest.Annotations[shpyrdv1.AnnotationBuildKey] != key {
		n := 1
		if latest != nil {
			n = buildNumber(latest) + 1
		}
		job := r.Config.desiredBuildJob(app, n, key)
		if err := controllerutil.SetControllerReference(app, job, r.Scheme); err != nil {
			return buildState{}, err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return buildState{}, fmt.Errorf("create build job: %w", err)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, "BuildRequested", "started Dockerfile build %s", job.Name)
		return buildState{LatestBuild: job.Name, Ready: "Unknown", Message: "build pending", LatestImage: lastImage(builds)}, nil
	}

	st := buildState{LatestBuild: latest.Name, Ready: "Unknown", Message: "building"}
	switch {
	case latest.Annotations[shpyrdv1.AnnotationBuildImage] != "":
		st.Ready, st.LatestImage = "True", latest.Annotations[shpyrdv1.AnnotationBuildImage]
		st.Revision = latest.Annotations[shpyrdv1.AnnotationBuildRevision]
	case latest.Annotations[shpyrdv1.AnnotationBuildFailure] != "":
		st.Ready, st.Message = "False", latest.Annotations[shpyrdv1.AnnotationBuildFailure]
		st.LatestImage = lastImage(builds[:len(builds)-1])
	case latest.Status.Succeeded > 0:
		res, msg := r.buildOutcome(ctx, latest)
		if res == nil {
			st.Ready, st.Message = "False", firstNonEmpty(msg, "build finished without reporting an image")
			if err := r.annotateJob(ctx, latest, map[string]string{shpyrdv1.AnnotationBuildFailure: st.Message}); err != nil {
				return st, err
			}
			st.LatestImage = lastImage(builds[:len(builds)-1])
			break
		}
		if err := r.annotateJob(ctx, latest, map[string]string{
			shpyrdv1.AnnotationBuildImage:    res.Image,
			shpyrdv1.AnnotationBuildRevision: res.Revision,
		}); err != nil {
			return st, err
		}
		st.Ready, st.LatestImage, st.Revision = "True", res.Image, res.Revision
		r.Recorder.Eventf(app, corev1.EventTypeNormal, "BuildSucceeded", "%s pushed %s", latest.Name, shortImage(res.Image))
	case jobFailed(latest):
		_, msg := r.buildOutcome(ctx, latest)
		st.Ready, st.Message = "False", firstNonEmpty(msg, "build failed")
		if err := r.annotateJob(ctx, latest, map[string]string{shpyrdv1.AnnotationBuildFailure: st.Message}); err != nil {
			return st, err
		}
		st.LatestImage = lastImage(builds[:len(builds)-1])
		r.Recorder.Eventf(app, corev1.EventTypeWarning, "BuildFailed", "%s: %s", latest.Name, st.Message)
	default:
		st.LatestImage = lastImage(builds[:len(builds)-1])
		if latest.Status.Active > 0 {
			st.Message = "building " + latest.Name
		}
	}
	if err := r.pruneBuildJobs(ctx, builds); err != nil {
		return st, err
	}
	return st, nil
}

// buildJobs returns the app's build Jobs sorted by build number.
func buildJobs(items []batchv1.Job) []batchv1.Job {
	var out []batchv1.Job
	for _, j := range items {
		if _, ok := j.Labels[shpyrdv1.LabelBuildNumber]; ok && j.DeletionTimestamp == nil {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return buildNumber(&out[i]) < buildNumber(&out[k]) })
	return out
}

func buildNumber(j *batchv1.Job) int {
	n, _ := strconv.Atoi(j.Labels[shpyrdv1.LabelBuildNumber])
	return n
}

// lastImage is the newest image among finished builds.
func lastImage(builds []batchv1.Job) string {
	for i := len(builds) - 1; i >= 0; i-- {
		if img := builds[i].Annotations[shpyrdv1.AnnotationBuildImage]; img != "" {
			return img
		}
	}
	return ""
}

func jobFailed(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// buildKey identifies the source and build settings a Job builds.
func buildKey(app *shpyrdv1.App) string {
	h := sha256.New()
	if src := app.Spec.Source; src != nil {
		switch {
		case src.Blob != nil:
			h.Write([]byte("blob:" + firstNonEmpty(src.Blob.SHA256, src.Blob.URL)))
		case src.Git != nil:
			h.Write([]byte("git:" + src.Git.URL + "#" + src.Git.Revision))
		}
		h.Write([]byte("subpath:" + src.SubPath))
	}
	if b := app.Spec.Build; b != nil {
		raw, _ := json.Marshal(b)
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// buildOutcome reads the build container's termination message: the JSON
// result on success, the log tail on failure.
func (r *AppReconciler) buildOutcome(ctx context.Context, job *batchv1.Job) (*buildResult, string) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{shpyrdv1.LabelBuild: job.Name}); err != nil || len(pods.Items) == 0 {
		return nil, ""
	}
	sort.Slice(pods.Items, func(i, k int) bool {
		return pods.Items[i].CreationTimestamp.After(pods.Items[k].CreationTimestamp.Time)
	})
	pod := pods.Items[0]
	for _, cs := range pod.Status.InitContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return nil, failureSummary("fetching the source failed", t.Message)
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != buildContainer || cs.State.Terminated == nil {
			continue
		}
		t := cs.State.Terminated
		if t.ExitCode == 0 {
			var res buildResult
			if err := json.Unmarshal([]byte(strings.TrimSpace(t.Message)), &res); err == nil && res.Image != "" {
				return &res, ""
			}
			return nil, "build finished without reporting an image"
		}
		return nil, failureSummary(fmt.Sprintf("exit %d", t.ExitCode), t.Message)
	}
	return nil, ""
}

// failureSummary keeps the most useful part of a log tail: the error lines
// when BuildKit printed any, otherwise the last lines, trimmed to a size
// that fits a status message.
func failureSummary(prefix, tail string) string {
	var lines, errs []string
	for _, l := range strings.Split(strings.TrimSpace(tail), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		lines = append(lines, l)
		if strings.HasPrefix(l, "error:") || strings.HasPrefix(l, "ERROR") || strings.Contains(l, " ERROR: ") {
			errs = append(errs, l)
		}
	}
	keep := lines
	if len(errs) > 0 {
		keep = errs
	}
	if n := 3; len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	msg := strings.Join(keep, " | ")
	if len(msg) > 400 {
		msg = msg[len(msg)-400:]
	}
	if msg == "" {
		return prefix
	}
	return prefix + ": " + msg
}

func (r *AppReconciler) annotateJob(ctx context.Context, job *batchv1.Job, ann map[string]string) error {
	patch := client.MergeFrom(job.DeepCopy())
	job.Annotations = mergeMaps(job.Annotations, ann)
	if err := r.Patch(ctx, job, patch); err != nil {
		return fmt.Errorf("annotate build job: %w", err)
	}
	return nil
}

// pruneBuildJobs deletes old finished builds beyond the history limits; the
// newest build is always kept.
func (r *AppReconciler) pruneBuildJobs(ctx context.Context, builds []batchv1.Job) error {
	if len(builds) == 0 {
		return nil
	}
	var succeeded, failed []batchv1.Job
	for _, j := range builds[:len(builds)-1] {
		switch {
		case j.Annotations[shpyrdv1.AnnotationBuildImage] != "":
			succeeded = append(succeeded, j)
		case j.Annotations[shpyrdv1.AnnotationBuildFailure] != "" || jobFailed(&j):
			failed = append(failed, j)
		}
	}
	del := func(list []batchv1.Job, keep int) error {
		for len(list) > keep {
			j := list[0]
			list = list[1:]
			if err := r.Delete(ctx, &j, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		return nil
	}
	if err := del(succeeded, successfulBuildsKept); err != nil {
		return err
	}
	return del(failed, failedBuildsKept)
}

// buildJobName names build n of an app.
func buildJobName(app *shpyrdv1.App, n int) string { return fmt.Sprintf("%s-build-%d", app.Name, n) }

// desiredBuildJob builds the Job for build number n.
func (c Config) desiredBuildJob(app *shpyrdv1.App, n int, key string) *batchv1.Job {
	name := buildJobName(app, n)
	labels := mergeMaps(commonLabels(app), map[string]string{shpyrdv1.LabelBuildNumber: strconv.Itoa(n)})
	podLabels := mergeMaps(commonLabels(app), map[string]string{shpyrdv1.LabelBuild: name})
	uid := ptr.To[int64](1000)
	unconfined := &corev1.SecurityContext{
		RunAsUser:  uid,
		RunAsGroup: uid,
		// Rootless BuildKit needs user namespaces and setuid newuidmap, so
		// the usual restrictions (seccomp, AppArmor, no privilege
		// escalation) cannot apply. It still runs unprivileged.
		SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
	}

	contextDir := workspaceDir + "/src"
	if sub := path.Clean("/" + app.Spec.Source.SubPath); sub != "/" {
		contextDir += sub
	}
	repo := c.imageTag(app)
	// Only an external registry without TLS is pushed to over plain HTTP;
	// the in-cluster registry's certificate is trusted through the bundle.
	insecure := ""
	if c.RegistryInsecure {
		insecure = ",registry.insecure=true"
	}
	args := []string{
		"build",
		"--progress", "plain",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + contextDir,
		"--local", "dockerfile=" + contextDir,
		"--opt", "filename=" + dockerfilePath(app),
		"--output", fmt.Sprintf("type=image,name=%s:b%d,push=true%s", repo, n, insecure),
		"--export-cache", fmt.Sprintf("type=registry,ref=%s:cache,mode=max%s", repo, insecure),
		"--import-cache", fmt.Sprintf("type=registry,ref=%s:cache%s", repo, insecure),
		"--metadata-file", "/tmp/meta.json",
	}
	if b := app.Spec.Build; b != nil {
		if b.Target != "" {
			args = append(args, "--opt", "target="+b.Target)
		}
		for _, e := range b.Env {
			args = append(args, "--opt", "build-arg:"+e.Name+"="+e.Value)
		}
	}

	// The script runs buildctl with the arguments above, then reports the
	// pushed digest as JSON in the termination message.
	buildScript := `buildctl-daemonless.sh "$@"
digest=$(sed -n 's/.*"containerimage.digest": *"\([^"]*\)".*/\1/p' /tmp/meta.json)
[ -n "$digest" ] || { echo "no image digest in metadata"; exit 1; }
revision=$(cat ` + workspaceDir + `/revision 2>/dev/null || true)
printf '{"image":"%s@%s","revision":"%s"}' "$IMAGE_REPO" "$digest" "$revision" > /dev/termination-log
echo "pushed $IMAGE_REPO@$digest"`

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   app.Namespace,
			Labels:      labels,
			Annotations: map[string]string{shpyrdv1.AnnotationBuildKey: key},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To[int32](0),
			ActiveDeadlineSeconds: ptr.To[int64](1800),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					EnableServiceLinks: ptr.To(false),
					SecurityContext:    &corev1.PodSecurityContext{FSGroup: uid},
					InitContainers: []corev1.Container{{
						Name:                     fetchContainer,
						Image:                    c.BuildKitImage,
						Command:                  []string{"sh", "-ec", fetchScript(app)},
						SecurityContext:          &corev1.SecurityContext{RunAsUser: uid, RunAsGroup: uid, AllowPrivilegeEscalation: ptr.To(false)},
						VolumeMounts:             []corev1.VolumeMount{{Name: "workspace", MountPath: workspaceDir}},
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					}},
					Containers: []corev1.Container{{
						Name:    buildContainer,
						Image:   c.BuildKitImage,
						Command: []string{"sh", "-ec", buildScript, "buildctl"},
						Args:    args,
						Env: append(append([]corev1.EnvVar{
							{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
							{Name: "IMAGE_REPO", Value: repo},
						}, c.buildKitAuthEnv()...), c.trustEnv()...),
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						}},
						SecurityContext: unconfined,
						VolumeMounts: append(append([]corev1.VolumeMount{
							{Name: "workspace", MountPath: workspaceDir},
							{Name: "buildkit", MountPath: "/home/user/.local/share/buildkit"},
						}, c.buildKitAuthMounts()...), c.trustMounts()...),
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					}},
					ImagePullSecrets: c.imagePullSecrets(),
					Volumes: append(append([]corev1.Volume{
						{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "buildkit", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					}, c.buildKitAuthVolumes()...), c.trustVolumes()...),
				},
			},
		},
	}
	return job
}

// dockerfilePath is the Dockerfile location relative to the context.
func dockerfilePath(app *shpyrdv1.App) string {
	if app.Spec.Build != nil && app.Spec.Build.Dockerfile != "" {
		return strings.TrimPrefix(path.Clean(app.Spec.Build.Dockerfile), "/")
	}
	return "Dockerfile"
}

// fetchScript downloads the uploaded archive or clones the git revision
// into the workspace and records the resolved revision.
func fetchScript(app *shpyrdv1.App) string {
	src := app.Spec.Source
	dst := workspaceDir + "/src"
	switch {
	case src.Blob != nil:
		return fmt.Sprintf(`mkdir -p %[1]s
wget -qO /tmp/source.tgz %[2]s
tar xzf /tmp/source.tgz -C %[1]s
printf '%%s' %[3]s > %[4]s/revision
echo "source ready"`, dst, shellQuote(src.Blob.URL), shellQuote(src.Blob.Ref), workspaceDir)
	case src.Git != nil:
		rev := firstNonEmpty(src.Git.Revision, "main")
		return fmt.Sprintf(`url=%[1]s
rev=%[2]s
if git clone --quiet --depth 1 --branch "$rev" "$url" %[3]s 2>/dev/null; then :; else
  # not a branch or tag: fetch the commit
  git init --quiet %[3]s
  git -C %[3]s fetch --quiet --depth 1 "$url" "$rev"
  git -C %[3]s checkout --quiet FETCH_HEAD
fi
git -C %[3]s rev-parse HEAD > %[4]s/revision
echo "source ready at $(cat %[4]s/revision)"`, shellQuote(src.Git.URL), shellQuote(rev), dst, workspaceDir)
	}
	return "echo 'no source'; exit 1"
}

// shellQuote single-quotes s for POSIX sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Registry credentials for BuildKit: the mirrored dockerconfigjson Secret
// mounted where the rootless user's docker config lives.
const dockerConfigDir = "/home/user/.docker"

func (c Config) buildKitAuthEnv() []corev1.EnvVar {
	if c.RegistrySecret == "" {
		return nil
	}
	return []corev1.EnvVar{{Name: "DOCKER_CONFIG", Value: dockerConfigDir}}
}

func (c Config) buildKitAuthMounts() []corev1.VolumeMount {
	if c.RegistrySecret == "" {
		return nil
	}
	return []corev1.VolumeMount{{Name: "registry-auth", MountPath: dockerConfigDir, ReadOnly: true}}
}

// The trust bundle for BuildKit (RFC-0059): buildkitd and buildctl are Go
// programs, so SSL_CERT_FILE makes them trust the in-cluster registry's
// certificate along with the public roots the bundle carries.
func (c Config) trustEnv() []corev1.EnvVar {
	if c.CABundle == "" {
		return nil
	}
	return []corev1.EnvVar{{Name: buildtrust.EnvVar, Value: buildtrust.MountPath + "/" + buildtrust.BundleKey}}
}

func (c Config) trustMounts() []corev1.VolumeMount {
	if c.CABundle == "" {
		return nil
	}
	return []corev1.VolumeMount{{Name: buildtrust.VolumeName, MountPath: buildtrust.MountPath, ReadOnly: true}}
}

func (c Config) trustVolumes() []corev1.Volume {
	if c.CABundle == "" {
		return nil
	}
	return []corev1.Volume{{Name: buildtrust.VolumeName, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: c.CABundle},
	}}}}
}

func (c Config) buildKitAuthVolumes() []corev1.Volume {
	if c.RegistrySecret == "" {
		return nil
	}
	return []corev1.Volume{{Name: "registry-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: c.RegistrySecret,
		Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
	}}}}
}

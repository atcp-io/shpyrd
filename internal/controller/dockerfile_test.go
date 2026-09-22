package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

const testDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func dockerfileApp() *shpyrdv1.App {
	return &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "dk", Namespace: "app-dk", Generation: 1},
		Spec: shpyrdv1.AppSpec{
			Source: &shpyrdv1.Source{Blob: &shpyrdv1.BlobSource{
				URL: "http://shpyrd-server.shpyrd-system.svc/api/sources/abc.tgz", SHA256: "abc123abc123abc123", Ref: "0123456789ab",
			}},
			Build: &shpyrdv1.Build{
				Strategy: shpyrdv1.StrategyDockerfile, Dockerfile: "deploy/Dockerfile", Target: "runtime",
				Env: []corev1.EnvVar{{Name: "NODE_ENV", Value: "production"}},
			},
		},
	}
}

func getJob(t *testing.T, c client.Client, ns, name string) *batchv1.Job {
	t.Helper()
	j := &batchv1.Job{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, j); err != nil {
		t.Fatalf("job %s: %v", name, err)
	}
	return j
}

// finishBuild marks the Job finished and creates its pod with the
// termination message the build container would have written.
func finishBuild(t *testing.T, c client.Client, job *batchv1.Job, exitCode int32, message string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x1", Namespace: job.Namespace, Labels: job.Spec.Template.Labels},
		Spec:       job.Spec.Template.Spec,
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{Name: buildContainer, State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: message},
			}}},
		},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if exitCode == 0 {
		job.Status.Succeeded = 1
	} else {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func TestDockerfileBuildJob(t *testing.T) {
	app := dockerfileApp()
	r, c := newTestReconciler(t, app)

	got := runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseBuilding || got.Status.LatestBuild != "dk-build-1" {
		t.Fatalf("phase = %q build=%q (%s)", got.Status.Phase, got.Status.LatestBuild, got.Status.Message)
	}
	img := &unstructured.Unstructured{}
	img.SetGroupVersionKind(KpackImageGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-dk", Name: "dk"}, img); err == nil {
		t.Error("no kpack Image must be created for Dockerfile builds")
	}

	job := getJob(t, c, "app-dk", "dk-build-1")
	if job.Labels[shpyrdv1.LabelBuildNumber] != "1" || job.Annotations[shpyrdv1.AnnotationBuildKey] == "" {
		t.Errorf("job labels/annotations: %v %v", job.Labels, job.Annotations)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Name != "dk" {
		t.Error("job must be owned by the app")
	}
	spec := job.Spec.Template.Spec
	if job.Spec.Template.Labels[shpyrdv1.LabelBuild] != "dk-build-1" {
		t.Errorf("build pods must carry %s: %v", shpyrdv1.LabelBuild, job.Spec.Template.Labels)
	}
	if len(spec.InitContainers) != 1 || !strings.Contains(spec.InitContainers[0].Command[2], "wget -qO /tmp/source.tgz 'http://shpyrd-server.shpyrd-system.svc/api/sources/abc.tgz'") {
		t.Errorf("fetch script: %+v", spec.InitContainers)
	}
	args := strings.Join(spec.Containers[0].Args, " ")
	for _, want := range []string{
		"--local context=/workspace/src",
		"--opt filename=deploy/Dockerfile",
		"--opt target=runtime",
		"--opt build-arg:NODE_ENV=production",
		"type=image,name=10.96.0.50:5000/apps/dk:b1,push=true,registry.insecure=true",
		"ref=10.96.0.50:5000/apps/dk:cache",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("buildctl args miss %q: %s", want, args)
		}
	}
	if spec.Containers[0].TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Error("build container must fall back to logs for the failure reason")
	}
	if sc := spec.Containers[0].SecurityContext; sc == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined || *sc.RunAsUser != 1000 {
		t.Errorf("rootless buildkit security context: %+v", sc)
	}

	// Same source again: no new build.
	runReconcile(t, r, app)
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("app-dk")); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d (%v)", len(jobs.Items), err)
	}

	// The build succeeds and reports its digest.
	finishBuild(t, c, job, 0, `{"image":"10.96.0.50:5000/apps/dk@`+testDigest+`","revision":""}`)
	got = runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseDeploying {
		t.Fatalf("phase = %q (%s), want Deploying", got.Status.Phase, got.Status.Message)
	}
	if got.Status.Image != "10.96.0.50:5000/apps/dk@"+testDigest {
		t.Errorf("status.image = %q", got.Status.Image)
	}
	if len(got.Status.Releases) != 1 || got.Status.Releases[0].Description != "Initial deploy 0123456789ab" {
		t.Errorf("releases = %+v", got.Status.Releases)
	}
	if getJob(t, c, "app-dk", "dk-build-1").Annotations[shpyrdv1.AnnotationBuildImage] == "" {
		t.Error("finished job must be annotated with its image")
	}

	// A new archive triggers build 2; its failure keeps the release and reports the log tail.
	app = got
	app.Spec.Source.Blob.SHA256, app.Spec.Source.Blob.Ref = "def456def456def456", "fedcba987654"
	if err := c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, app)
	if got.Status.LatestBuild != "dk-build-2" || got.Status.Phase != shpyrdv1.PhaseBuilding {
		t.Fatalf("build=%q phase=%q", got.Status.LatestBuild, got.Status.Phase)
	}
	markDeploymentReady(t, c, "app-dk", "dk-web", 1)
	finishBuild(t, c, getJob(t, c, "app-dk", "dk-build-2"), 1, "#5 [2/3] RUN npm ci\n#5 ERROR: process \"/bin/sh -c npm ci\" did not complete successfully: exit code: 1\nerror: failed to solve: process did not complete successfully\n")
	got = runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseFailed || !strings.Contains(got.Status.Message, "failed to solve") {
		t.Fatalf("phase = %q (%s)", got.Status.Phase, got.Status.Message)
	}
	if got.Status.Image != "10.96.0.50:5000/apps/dk@"+testDigest || len(got.Status.Releases) != 1 {
		t.Error("previous release must keep running after a failed build")
	}
}

func TestDockerfileGitFetchAndProcesses(t *testing.T) {
	app := dockerfileApp()
	app.Spec.Source = &shpyrdv1.Source{Git: &shpyrdv1.GitSource{URL: "https://github.com/o/r.git", Revision: "v1.2"}, SubPath: "services/api"}
	app.Spec.Processes = map[string]shpyrdv1.Process{"web": {}, "worker": {}}
	r, c := newTestReconciler(t, app)

	got := runReconcile(t, r, app)
	job := getJob(t, c, "app-dk", "dk-build-1")
	fetch := job.Spec.Template.Spec.InitContainers[0].Command[2]
	if !strings.Contains(fetch, "git clone --quiet --depth 1 --branch \"$rev\"") || !strings.Contains(fetch, "rev='v1.2'") {
		t.Errorf("git fetch script: %s", fetch)
	}
	if args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " "); !strings.Contains(args, "--local context=/workspace/src/services/api") {
		t.Errorf("subPath must select the context dir: %s", args)
	}

	// Finish with a resolved revision; the worker lacks a command so the
	// rollout is refused with a clear message.
	finishBuild(t, c, job, 0, `{"image":"10.96.0.50:5000/apps/dk@`+testDigest+`","revision":"9f8e7d6c5b4a3210"}`)
	got = runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseFailed || !strings.Contains(got.Status.Message, `process "worker" needs a command`) {
		t.Fatalf("phase = %q (%s)", got.Status.Phase, got.Status.Message)
	}

	app = got
	app.Spec.Processes["worker"] = shpyrdv1.Process{Command: []string{"node", "worker.js"}}
	if err := c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseDeploying {
		t.Fatalf("phase = %q (%s)", got.Status.Phase, got.Status.Message)
	}
	if len(got.Status.Releases) != 1 || got.Status.Releases[0].Source != "9f8e7d6c5b4a" {
		t.Errorf("release source should be the resolved commit: %+v", got.Status.Releases)
	}
}

func TestBuildKeyAndFailureSummary(t *testing.T) {
	a := dockerfileApp()
	b := dockerfileApp()
	if buildKey(a) != buildKey(b) {
		t.Error("same spec must give the same key")
	}
	b.Spec.Build.Target = "debug"
	if buildKey(a) == buildKey(b) {
		t.Error("build settings are part of the key")
	}
	c := dockerfileApp()
	c.Spec.Source.Blob.SHA256 = "other"
	if buildKey(a) == buildKey(c) {
		t.Error("the source is part of the key")
	}

	got := failureSummary("exit 1", "#1 [internal] load\n#2 DONE 0.1s\n\n#3 ERROR: not found\n   2 | >>> RUN false\n--------------------\nerror: failed to solve: not found\n")
	if got != "exit 1: #3 ERROR: not found | error: failed to solve: not found" {
		t.Errorf("summary = %q", got)
	}
	if got := failureSummary("exit 2", "a\nb\nc\nd"); got != "exit 2: b | c | d" {
		t.Errorf("fallback summary = %q", got)
	}
	if failureSummary("x", "") != "x" {
		t.Error("empty tail keeps the prefix")
	}
	if shellQuote("it's") != `'it'\''s'` {
		t.Errorf("shellQuote = %s", shellQuote("it's"))
	}
}

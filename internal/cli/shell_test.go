package cli

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
)

func testApp(withSource bool) *shpyrdv1.App {
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "app-demo"},
		Spec:       shpyrdv1.AppSpec{Env: []corev1.EnvVar{{Name: "GREETING", Value: "hi"}}},
	}
	if withSource {
		app.Spec.Source = &shpyrdv1.Source{Git: &shpyrdv1.GitSource{URL: "https://example.com/repo.git", Revision: "main"}}
	}
	return app
}

func TestRunPodBuildpackImage(t *testing.T) {
	res := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}}
	pod := runPod(testApp(true), "10.96.0.50:5000/demo@sha256:abc", []string{"sh", "-c", "echo $X"}, res, true, true)

	if !strings.HasPrefix(pod.Name, "demo-run-") || len(pod.Name) != len("demo-run-")+6 {
		t.Errorf("unexpected name %q", pod.Name)
	}
	if pod.Labels[shpyrdv1.LabelProcess] != "run" || pod.Labels[shpyrdv1.LabelApp] != "demo" {
		t.Errorf("labels: %v", pod.Labels)
	}
	c := pod.Spec.Containers[0]
	// The launcher loads the buildpack environment; "--" stops it from
	// re-joining the arguments through bash -c.
	want := []string{cnbLauncher, "--", "sh", "-c", "echo $X"}
	if !reflect.DeepEqual(c.Command, want) {
		t.Errorf("command = %v, want %v", c.Command, want)
	}
	if !c.TTY || !c.Stdin || !c.StdinOnce {
		t.Error("interactive runs need stdin (once) and a tty")
	}
	// Globals first, then config vars, then bound vars: the same order as
	// the deployed processes (RFC-0016).
	if len(c.EnvFrom) != 3 || c.EnvFrom[0].SecretRef.Name != "shpyrd-global-env" || c.EnvFrom[1].SecretRef.Name != "demo-env" || c.EnvFrom[1].SecretRef.Optional == nil || !*c.EnvFrom[1].SecretRef.Optional {
		t.Errorf("env sources: %+v", c.EnvFrom)
	}
	if c.Env[0].Name != "SHPYRD_RUN" || c.Env[1].Name != "GREETING" {
		t.Errorf("env: %+v", c.Env)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || pod.Spec.ActiveDeadlineSeconds == nil {
		t.Error("one-off pods must not restart and must have a deadline")
	}
	if !reflect.DeepEqual(c.Resources, res) {
		t.Errorf("resources = %v", c.Resources)
	}
}

func TestRunPodPrebuiltImage(t *testing.T) {
	pod := runPod(testApp(false), "ghcr.io/org/tool:1", []string{"tool", "--help"}, corev1.ResourceRequirements{}, false, true)
	c := pod.Spec.Containers[0]
	if !reflect.DeepEqual(c.Command, []string{"tool", "--help"}) {
		t.Errorf("prebuilt images run the command directly, got %v", c.Command)
	}
	dk := testApp(true)
	dk.Spec.Build = &shpyrdv1.Build{Strategy: shpyrdv1.StrategyDockerfile}
	if got := runPod(dk, "x", []string{"tool"}, corev1.ResourceRequirements{}, false, true).Spec.Containers[0].Command; !reflect.DeepEqual(got, []string{"tool"}) {
		t.Errorf("Dockerfile images run the command directly, got %v", got)
	}
	if c.TTY {
		t.Error("non-interactive runs must not allocate a tty")
	}
	if !c.Stdin || !c.StdinOnce {
		t.Error("stdin stays attached so input can be piped; stdinOnce keeps output flowing after EOF")
	}
	if d := runPod(testApp(false), "x", []string{"tool"}, corev1.ResourceRequirements{}, false, false).Spec.Containers[0]; d.Stdin || d.TTY {
		t.Error("detached runs do not attach stdin")
	}
}

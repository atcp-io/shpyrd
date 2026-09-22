package cli

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kexec "k8s.io/client-go/util/exec"

	shpyrdv1 "shpyrd/api/v1alpha1"
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
	pod := runPod(testApp(true), "10.96.0.50:5000/demo@sha256:abc", []string{"sh", "-c", "echo $X"}, res, true)

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
	if !c.TTY || !c.Stdin {
		t.Error("interactive runs need stdin and a tty")
	}
	if c.EnvFrom[0].SecretRef.Name != "demo-env" || c.EnvFrom[0].SecretRef.Optional == nil || !*c.EnvFrom[0].SecretRef.Optional {
		t.Errorf("config vars secret: %+v", c.EnvFrom)
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
	pod := runPod(testApp(false), "ghcr.io/org/tool:1", []string{"tool", "--help"}, corev1.ResourceRequirements{}, false)
	c := pod.Spec.Containers[0]
	if !reflect.DeepEqual(c.Command, []string{"tool", "--help"}) {
		t.Errorf("prebuilt images run the command directly, got %v", c.Command)
	}
	if c.TTY {
		t.Error("non-interactive runs must not allocate a tty")
	}
	if !c.Stdin {
		t.Error("stdin stays attached so input can be piped")
	}
}

func TestRemoteExit(t *testing.T) {
	err := remoteExit(kexec.CodeExitError{Err: errors.New("command terminated with exit code 3"), Code: 3})
	if ExitCode(err) != 3 {
		t.Errorf("exit code = %d, want 3", ExitCode(err))
	}
	plain := errors.New("boom")
	if remoteExit(plain) != plain || ExitCode(plain) != 1 {
		t.Error("other errors pass through and exit 1")
	}
	if remoteExit(nil) != nil {
		t.Error("nil stays nil")
	}
}

func TestIsNotFound(t *testing.T) {
	for _, m := range []string{
		`OCI runtime exec failed: exec failed: unable to start container process: exec: "bash": executable file not found in $PATH: unknown`,
		"command terminated with exit code 126",
		"command terminated with exit code 127",
	} {
		if !isNotFound(errors.New(m)) {
			t.Errorf("should be treated as missing executable: %s", m)
		}
	}
	if isNotFound(errors.New("command terminated with exit code 2")) {
		t.Error("ordinary failures are not missing executables")
	}
}

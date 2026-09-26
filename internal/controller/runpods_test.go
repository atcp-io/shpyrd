package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
)

func runPod(name, app string, phase corev1.PodPhase, finished time.Time) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "app-" + app,
			Labels: map[string]string{shpyrdv1.LabelApp: app, shpyrdv1.LabelProcess: runProcess},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "x"}}},
		Status: corev1.PodStatus{Phase: phase},
	}
	if !finished.IsZero() {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.NewTime(finished)},
		}}}
	}
	return p
}

func TestGCRunPods(t *testing.T) {
	app := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "app-demo"}}
	now := time.Now()
	old := runPod("demo-run-old", "demo", corev1.PodSucceeded, now.Add(-runPodRetention-time.Minute))
	recent := runPod("demo-run-recent", "demo", corev1.PodFailed, now.Add(-time.Minute))
	running := runPod("demo-run-live", "demo", corev1.PodRunning, time.Time{})
	other := runPod("other-run-old", "other", corev1.PodSucceeded, now.Add(-time.Hour))
	other.Namespace = "app-demo"
	r, c := newTestReconciler(t, app, old, recent, running, other)

	next, err := r.gcRunPods(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if next <= 0 || next > runPodRetention {
		t.Fatalf("expected a requeue within the retention period for the recent pod, got %v", next)
	}
	exists := func(name string) bool {
		err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: name}, &corev1.Pod{})
		if apierrors.IsNotFound(err) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	}
	if exists("demo-run-old") {
		t.Error("finished pod past retention should be deleted")
	}
	if !exists("demo-run-recent") {
		t.Error("recently finished pod should be kept")
	}
	if !exists("demo-run-live") {
		t.Error("running pod should be kept")
	}
	if !exists("other-run-old") {
		t.Error("pods of other projects must not be touched")
	}
}

func TestRunPodToApp(t *testing.T) {
	reqs := runPodToApp(context.Background(), runPod("demo-run-1", "demo", corev1.PodRunning, time.Time{}))
	if len(reqs) != 1 || reqs[0].Name != "demo" || reqs[0].Namespace != "app-demo" {
		t.Fatalf("unexpected mapping: %+v", reqs)
	}
	if !isRunPod.Create(createEvent(runPod("demo-run-1", "demo", corev1.PodRunning, time.Time{}))) {
		t.Error("run pods should pass the predicate")
	}
	web := runPod("demo-web-1", "demo", corev1.PodRunning, time.Time{})
	web.Labels[shpyrdv1.LabelProcess] = "web"
	if isRunPod.Create(createEvent(web)) {
		t.Error("process pods should not pass the predicate")
	}
}

func createEvent(p *corev1.Pod) event.CreateEvent { return event.CreateEvent{Object: p} }

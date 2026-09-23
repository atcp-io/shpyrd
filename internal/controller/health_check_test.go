package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ptr "k8s.io/utils/ptr"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// RFC-0019: probes, rollout strategy and graceful shutdown.

func TestProbesByProcessType(t *testing.T) {
	app := sampleApp("probes")
	app.Spec.Image = "registry.test/p@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	port := ptr.To[int32](9090)
	app.Spec.Processes = map[string]shpyrdv1.Process{
		"web":    {},
		"api":    {Port: port},
		"worker": {},
	}
	r, c := newTestReconciler(t, app)
	runReconcile(t, r, app)

	get := func(name string) *appsv1.Deployment {
		d := &appsv1.Deployment{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-probes", Name: "probes-" + name}, d); err != nil {
			t.Fatalf("%s Deployment: %v", name, err)
		}
		return d
	}
	container := func(d *appsv1.Deployment) corev1.Container { return d.Spec.Template.Spec.Containers[0] }

	// web: HTTP GET / on 8080.
	web := container(get("web"))
	if rp := web.ReadinessProbe; rp == nil || rp.HTTPGet == nil || rp.HTTPGet.Path != "/" || rp.HTTPGet.Port.IntValue() != 8080 {
		t.Errorf("web readiness = %+v", web.ReadinessProbe)
	}
	if web.LivenessProbe == nil || web.StartupProbe == nil {
		t.Error("web must have liveness and startup probes")
	}
	if web.Lifecycle == nil || web.Lifecycle.PreStop == nil {
		t.Error("web must have a preStop lifecycle hook")
	}
	if web.LivenessProbe.FailureThreshold <= web.ReadinessProbe.FailureThreshold {
		t.Error("liveness must have a higher failure threshold than readiness")
	}
	// web Deployment must use RollingUpdate.
	if d := get("web"); d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Errorf("web strategy = %v", d.Spec.Strategy.Type)
	}
	// terminationGracePeriodSeconds must be set.
	if p := get("web").Spec.Template.Spec.TerminationGracePeriodSeconds; p == nil || *p < 10 {
		t.Errorf("terminationGracePeriodSeconds = %v", p)
	}

	// api (explicit port 9090): TCP.
	api := container(get("api"))
	if rp := api.ReadinessProbe; rp == nil || rp.TCPSocket == nil || rp.TCPSocket.Port.IntValue() != 9090 {
		t.Errorf("api readiness = %+v", api.ReadinessProbe)
	}

	// worker (no port): no probe at all.
	worker := container(get("worker"))
	if worker.ReadinessProbe != nil || worker.LivenessProbe != nil || worker.StartupProbe != nil {
		t.Errorf("worker must have no probes: %+v", worker.ReadinessProbe)
	}
}

func TestCustomHealthCheck(t *testing.T) {
	app := sampleApp("custom")
	app.Spec.Image = "registry.test/c@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	app.Spec.Processes = map[string]shpyrdv1.Process{
		"web": {HealthCheck: &shpyrdv1.HealthCheck{Path: "/healthz", Interval: "5s", GracePeriod: "60s", ShutdownDelay: "10s"}},
	}
	r, c := newTestReconciler(t, app)
	runReconcile(t, r, app)
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-custom", Name: "custom-web"}, dep); err != nil {
		t.Fatal(err)
	}
	ct := dep.Spec.Template.Spec.Containers[0]
	if ct.ReadinessProbe.HTTPGet.Path != "/healthz" || ct.ReadinessProbe.PeriodSeconds != 5 {
		t.Errorf("custom path/interval: %+v", ct.ReadinessProbe)
	}
	if ct.StartupProbe.FailureThreshold != 12 { // 60s / 5s
		t.Errorf("startup failure threshold: %d (want 12)", ct.StartupProbe.FailureThreshold)
	}
	tgp := dep.Spec.Template.Spec.TerminationGracePeriodSeconds
	if tgp == nil || *tgp != 20 { // 10 + 5 + 5
		t.Errorf("terminationGracePeriodSeconds = %v (want 20)", tgp)
	}
	// Disabled: no probes.
	app2 := sampleApp("disabled")
	app2.Spec.Image = "registry.test/d@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	app2.Spec.Processes = map[string]shpyrdv1.Process{
		"web": {HealthCheck: &shpyrdv1.HealthCheck{Disabled: true}},
	}
	r2, c2 := newTestReconciler(t, app2)
	runReconcile(t, r2, app2)
	dep2 := &appsv1.Deployment{}
	if err := c2.Get(context.Background(), types.NamespacedName{Namespace: "app-disabled", Name: "disabled-web"}, dep2); err != nil {
		t.Fatal(err)
	}
	ct2 := dep2.Spec.Template.Spec.Containers[0]
	if ct2.ReadinessProbe != nil || ct2.LivenessProbe != nil {
		t.Errorf("disabled: probes must be nil, got %+v", ct2.ReadinessProbe)
	}
}

func TestRolloutStrategyWithVolume(t *testing.T) {
	app := sampleApp("vol")
	app.Spec.Image = "registry.test/v@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	app.Spec.Processes = map[string]shpyrdv1.Process{"web": {Volumes: []shpyrdv1.VolumeMount{{Name: "data", Path: "/data"}}}}
	vol := testVolume("app-vol", "1Gi", corev1.ReadWriteOnce)
	vol.Namespace = "app-vol"
	vol.Name = "data"
	r, c := newTestReconciler(t, app, vol)
	runReconcile(t, r, app)
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-vol", Name: "vol-web"}, dep); err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("volume-pinned process must use Recreate, got %v", dep.Spec.Strategy.Type)
	}
}

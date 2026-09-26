package api

import (
	"encoding/json"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/ext"
)

// appPod is a pod of a project as the controller labels them.
func appPod(app, process, name string, phase corev1.PodPhase, ready bool) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "app-" + app,
			Labels:    map[string]string{shpyrdv1.LabelApp: app, shpyrdv1.LabelProcess: process},
		},
		Status: corev1.PodStatus{
			Phase:      phase,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
		},
	}
}

func TestListInstances(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog},
		appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true),
		appPod("blog", "web", "blog-web-bbb", corev1.PodRunning, false),
		appPod("blog", "worker", "blog-worker-ccc", corev1.PodRunning, true),
		// A one-off command's pod (RFC-0024) shares the namespace and must
		// never be offered a shell.
		appPod("blog", runProcess, "blog-run-ddd", corev1.PodRunning, true),
		// Not yet running: no process to attach to.
		appPod("blog", "web", "blog-web-eee", corev1.PodPending, false),
	)

	rec := do(t, s, "GET", "/api/projects/blog/instances", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("instances: %d %s", rec.Code, rec.Body.String())
	}
	var got []Instance
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, i := range got {
		names = append(names, i.Name)
	}
	if len(names) != 3 || names[0] != "web.1" || names[1] != "web.2" || names[2] != "worker.1" {
		t.Fatalf("instances = %v, want [web.1 web.2 worker.1]", names)
	}
	if got[0].Pod != "blog-web-aaa" || !got[0].Ready || got[1].Ready {
		t.Errorf("readiness or pod wrong: %+v", got[:2])
	}
	if got[2].Process != "worker" {
		t.Errorf("process = %q, want worker", got[2].Process)
	}
}

func TestListInstancesNeedsExec(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog}, appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true))
	s.authz.TTL = 1

	// Enforcement starts with the first team; then a viewer may read the
	// project but may not look for somewhere to run commands.
	if rec := do(t, s, "POST", "/api/teams", `{"name":"ops","members":["ops@example.test"],"platformRole":"platform-admin"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/blog/members", `{"role":"viewer","user":"viewer@example.test"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("add viewer: %d %s", rec.Code, rec.Body.String())
	}
	sid, _ := signIn(t, s, ext.Identity{Subject: "u2", Email: "viewer@example.test", Provider: "local"})
	if rec := doCookie(t, s, "GET", "/api/projects/blog/instances", "", sid, ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer instances: %d, want 403", rec.Code)
	}
}

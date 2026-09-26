package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

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

// buildPod is shaped like an image-build pod (RFC-0024's builder): it carries
// the app label but, unlike appPod, no process label at all. That is exactly
// what "process!=run" alone would still match, since a "!=" selector also
// matches objects missing the key.
func buildPod(app, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "app-" + app,
			Labels:    map[string]string{shpyrdv1.LabelApp: app, shpyrdv1.LabelBuild: name},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestListInstancesExcludesBuildPods(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog},
		appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true),
		buildPod("blog", "blog-build-3-xyz"),
	)

	rec := do(t, s, "GET", "/api/projects/blog/instances", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("instances: %d %s", rec.Code, rec.Body.String())
	}
	var got []Instance
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// Build pods run with unconfined seccomp/AppArmor for rootless
	// BuildKit; offering one a shell would be worse than the run-pod case.
	if len(got) != 1 || got[0].Name != "web.1" || got[0].Pod != "blog-web-aaa" {
		t.Fatalf("instances = %+v, want just web.1/blog-web-aaa", got)
	}
}

func TestListInstancesNamesAgreeDuringRollout(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	terminating := appPod("blog", "web", "blog-web-old", corev1.PodRunning, true)
	terminating.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	deletedAt := metav1.Now()
	terminating.DeletionTimestamp = &deletedAt
	surviving := appPod("blog", "web", "blog-web-new", corev1.PodRunning, true)
	surviving.CreationTimestamp = metav1.NewTime(time.Now())

	s, _ := newTestServer(t, nil, []client.Object{blog}, terminating, surviving)

	rec := do(t, s, "GET", "/api/projects/blog/instances", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("instances: %d %s", rec.Code, rec.Body.String())
	}
	var got []Instance
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// The older, terminating pod held web.1 (it sorts first by creation
	// time); the log viewer names it that way too, so the surviving pod
	// must stay web.2 here rather than being renumbered down to web.1.
	if len(got) != 1 || got[0].Name != "web.2" || got[0].Pod != "blog-web-new" {
		t.Fatalf("instances = %+v, want just web.2/blog-web-new", got)
	}
}

func TestMintShellTicket(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog}, appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true))

	rec := do(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var body struct{ Ticket string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Ticket == "" {
		t.Fatal("no ticket in the response")
	}

	// An instance that is not there cannot be given a ticket: the terminal
	// would otherwise open and then fail with nothing useful to say.
	if rec := do(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.9", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown instance: %d, want 404", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects/blog/shell/ticket", "", true); rec.Code != http.StatusBadRequest {
		t.Errorf("missing instance: %d, want 400", rec.Code)
	}
}

func TestMintShellTicketRefusesSecondShell(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog}, appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true))

	// A shell is already live for this caller: the admin token's identity.
	s.shells.claim(actorKey(ext.Identity{Subject: "admin-token", Provider: "token"}), "blog")
	rec := do(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", true)
	if rec.Code != http.StatusConflict {
		t.Errorf("second shell: %d, want 409", rec.Code)
	}
}

func TestMintShellTicketNeedsExec(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog}, appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true))
	s.authz.TTL = 1
	if rec := do(t, s, "POST", "/api/teams", `{"name":"ops","members":["ops@example.test"],"platformRole":"platform-admin"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/blog/members", `{"role":"viewer","user":"viewer@example.test"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("add viewer: %d %s", rec.Code, rec.Body.String())
	}
	sid, csrf := signIn(t, s, ext.Identity{Subject: "u2", Email: "viewer@example.test", Provider: "local"})
	if rec := doCookie(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", sid, csrf); rec.Code != http.StatusForbidden {
		t.Errorf("viewer mint: %d, want 403", rec.Code)
	}
	// Being a POST, it is also a CSRF-protected route for cookie callers.
	if rec := doCookie(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", sid, ""); rec.Code == http.StatusOK {
		t.Error("a cookie caller without the CSRF header must not mint a ticket")
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

func TestMintShellTicketIsThrottled(t *testing.T) {
	// The ticket is the cheapest way to make this single replica do expensive
	// work: one holder of project.exec looping mint-then-connect costs two pod
	// LISTs, up to four pods/exec calls against a live pod and two audit Events
	// per cycle. A person opens a terminal a handful of times a minute, so the
	// budget is generous for every real use and still a ceiling.
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog}, appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true))

	for i := 0; i < shellMintsPerMinute; i++ {
		if rec := do(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", true); rec.Code != http.StatusOK {
			t.Fatalf("mint %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := do(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", true)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("mint past the budget: %d %s, want 429", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 should say when to come back")
	}
}

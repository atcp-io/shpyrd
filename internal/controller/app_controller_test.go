package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/kube"
)

func newTestReconciler(t *testing.T, objs ...client.Object) (*AppReconciler, client.Client) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&shpyrdv1.App{}, &shpyrdv1.Volume{}, &shpyrdv1.Postgres{}, &shpyrdv1.Redis{}, &shpyrdv1.ObjectBucket{}).
		Build()
	r := &AppReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(100),
		Config:    Config{Domain: "example.test", HTTPSPort: "8443", RegistryHost: "10.96.0.50:5000", RegistryInsecure: true}.Defaults(),
	}
	return r, c
}

func runReconcile(t *testing.T, r *AppReconciler, app *shpyrdv1.App) *shpyrdv1.App {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: app.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	out := &shpyrdv1.App{}
	if err := r.Get(context.Background(), req.NamespacedName, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func markDeploymentReady(t *testing.T, c client.Client, ns, name string, replicas int32) {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, d); err != nil {
		t.Fatalf("deployment %s: %v", name, err)
	}
	d.Status.ObservedGeneration = d.Generation
	d.Status.ReadyReplicas = replicas
	d.Status.UpdatedReplicas = replicas
	d.Status.AvailableReplicas = replicas
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func TestReconcilePinnedImage(t *testing.T) {
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web1", Namespace: "app-web1", Generation: 1},
		Spec: shpyrdv1.AppSpec{
			Image: "ghcr.io/example/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Processes: map[string]shpyrdv1.Process{
				"web":    {Replicas: ptr.To[int32](2)},
				"worker": {},
			},
		},
	}
	r, c := newTestReconciler(t, app)

	got := runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseDeploying {
		t.Fatalf("phase = %q, want Deploying (%s)", got.Status.Phase, got.Status.Message)
	}
	if got.Status.URL != "https://web1.example.test:8443" {
		t.Errorf("url = %q", got.Status.URL)
	}
	if len(got.Status.Releases) != 1 || got.Status.Releases[0].Number != 1 {
		t.Fatalf("releases = %+v, want v1", got.Status.Releases)
	}

	// Workloads rendered as expected.
	web := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1-web"}, web); err != nil {
		t.Fatal(err)
	}
	ct := web.Spec.Template.Spec.Containers[0]
	if ct.Image != app.Spec.Image || *web.Spec.Replicas != 2 {
		t.Errorf("web deployment image/replicas wrong: %s %d", ct.Image, *web.Spec.Replicas)
	}
	if len(ct.Command) != 0 {
		t.Errorf("web must use the image entrypoint, got command %v", ct.Command)
	}
	if !hasEnv(ct.Env, "PORT", "8080") {
		t.Errorf("web must get PORT=8080, env=%v", ct.Env)
	}
	worker := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1-worker"}, worker); err != nil {
		t.Fatal(err)
	}
	if got := worker.Spec.Template.Spec.Containers[0].Command; len(got) != 1 || got[0] != "/cnb/process/worker" {
		t.Errorf("worker command = %v", got)
	}
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1-web"}, svc); err != nil {
		t.Errorf("web service missing: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1-worker"}, &corev1.Service{}); err == nil {
		t.Errorf("worker must not get a service")
	}
	ing := &networkingv1.Ingress{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1"}, ing); err != nil {
		t.Fatal(err)
	}
	// Certificates are explicit objects (RFC-0034): the Ingress names the
	// host's TLS Secret and carries no cert-manager annotation.
	if ing.Spec.Rules[0].Host != "web1.example.test" || ing.Annotations["cert-manager.io/cluster-issuer"] != "" || ing.Spec.TLS[0].SecretName != certificateSecretName(app, "web1.example.test") {
		t.Errorf("ingress host/tls wrong: %+v %+v", ing.Spec.Rules[0].Host, ing.Spec.TLS)
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(CertificateGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: certificateSecretName(app, "web1.example.test")}, cert); err != nil {
		t.Fatalf("certificate for the default host: %v", err)
	}
	if names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames"); len(names) != 1 || names[0] != "web1.example.test" {
		t.Errorf("certificate dnsNames = %v", names)
	}
	// With the platform wildcard as the default certificate (RFC-0061) the
	// Ingress keeps its TLS host but carries no certificate of its own.
	wc := r.Config
	wc.WildcardTLS = true
	wcIng := ing.DeepCopy()
	wc.mutateIngress(app, wcIng)
	if wcIng.Spec.TLS[0].SecretName != "" || len(wcIng.Spec.TLS[0].Hosts) != 1 {
		t.Errorf("wildcard ingress = %+v", wcIng.Spec.TLS)
	}

	// Rollout completes -> Running.
	markDeploymentReady(t, c, "app-web1", "web1-web", 2)
	markDeploymentReady(t, c, "app-web1", "web1-worker", 1)
	got = runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseRunning {
		t.Fatalf("phase = %q (%s), want Running", got.Status.Phase, got.Status.Message)
	}
	if len(got.Status.Releases) != 1 {
		t.Errorf("a completed rollout must not add a release, got %d", len(got.Status.Releases))
	}

	// Config change -> new release, pod template hash changes.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "web1-env", Namespace: "app-web1"}, Data: map[string][]byte{"A": []byte("1")}}
	if err := c.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	before := web.Spec.Template.Annotations[shpyrdv1.AnnotationConfigHash]
	got = runReconcile(t, r, app)
	if len(got.Status.Releases) != 2 || got.Status.Releases[1].Description != "Set A config var" {
		t.Fatalf("releases after config change = %+v", got.Status.Releases)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1-web"}, web); err != nil {
		t.Fatal(err)
	}
	if web.Spec.Template.Annotations[shpyrdv1.AnnotationConfigHash] == before {
		t.Errorf("config hash annotation did not change")
	}

	// Removing a process type deletes its workloads.
	cur := &shpyrdv1.App{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1"}, cur); err != nil {
		t.Fatal(err)
	}
	delete(cur.Spec.Processes, "worker")
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r, app)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1-worker"}, &appsv1.Deployment{}); err == nil {
		t.Errorf("worker deployment should have been deleted")
	}
}

func TestReconcileSourceBuild(t *testing.T) {
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "app-src", Generation: 1},
		Spec: shpyrdv1.AppSpec{
			Source: &shpyrdv1.Source{Git: &shpyrdv1.GitSource{URL: "https://github.com/example/repo", Revision: "main"}, SubPath: "svc"},
		},
	}
	r, c := newTestReconciler(t, app)

	// First pass creates the kpack Image and reports Building.
	got := runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseBuilding {
		t.Fatalf("phase = %q (%s), want Building", got.Status.Phase, got.Status.Message)
	}
	img := &unstructured.Unstructured{}
	img.SetGroupVersionKind(KpackImageGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-src", Name: "src"}, img); err != nil {
		t.Fatalf("kpack image: %v", err)
	}
	if tag, _, _ := unstructured.NestedString(img.Object, "spec", "tag"); tag != "10.96.0.50:5000/apps/src" {
		t.Errorf("tag = %q", tag)
	}
	if sub, _, _ := unstructured.NestedString(img.Object, "spec", "source", "subPath"); sub != "svc" {
		t.Errorf("subPath = %q", sub)
	}
	if len(img.GetOwnerReferences()) != 1 || img.GetOwnerReferences()[0].Name != "src" {
		t.Errorf("kpack image must be owned by the app")
	}

	// kpack finishes a build.
	_ = unstructured.SetNestedField(img.Object, "10.96.0.50:5000/apps/src@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "status", "latestImage")
	_ = unstructured.SetNestedField(img.Object, "src-build-1", "status", "latestBuildRef")
	_ = unstructured.SetNestedField(img.Object, int64(img.GetGeneration()), "status", "observedGeneration")
	_ = unstructured.SetNestedSlice(img.Object, []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}, "status", "conditions")
	if err := c.Update(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseDeploying {
		t.Fatalf("phase = %q (%s), want Deploying", got.Status.Phase, got.Status.Message)
	}
	if got.Status.Image == "" || got.Status.LatestBuild != "src-build-1" {
		t.Errorf("status image/build not recorded: %+v", got.Status)
	}
	if len(got.Status.Releases) != 1 {
		t.Fatalf("releases = %+v", got.Status.Releases)
	}
	markDeploymentReady(t, c, "app-src", "src-web", 1)
	got = runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseRunning {
		t.Fatalf("phase = %q (%s), want Running", got.Status.Phase, got.Status.Message)
	}

	// A failing rebuild keeps the old release running but surfaces Failed.
	_ = unstructured.SetNestedSlice(img.Object, []interface{}{map[string]interface{}{"type": "Ready", "status": "False", "message": "detect failed"}}, "status", "conditions")
	if err := c.Update(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, app)
	if got.Status.Phase != shpyrdv1.PhaseFailed || got.Status.Image == "" {
		t.Fatalf("phase = %q image=%q, want Failed with image kept", got.Status.Phase, got.Status.Image)
	}
}

func TestConfigHashStable(t *testing.T) {
	app := &shpyrdv1.App{Spec: shpyrdv1.AppSpec{Env: []corev1.EnvVar{{Name: "B", Value: "2"}, {Name: "A", Value: "1"}}}}
	sec := &corev1.Secret{Data: map[string][]byte{"Y": []byte("y"), "X": []byte("x")}}
	sz := map[string]string{"web": "shared-s"}
	h1 := configHash(app, sec, sz, nil, nil)
	app.Spec.Env[0], app.Spec.Env[1] = app.Spec.Env[1], app.Spec.Env[0]
	if h2 := configHash(app, sec, sz, nil, nil); h1 != h2 {
		t.Errorf("hash must not depend on env order: %s != %s", h1, h2)
	}
	sec.Data["X"] = []byte("changed")
	if h3 := configHash(app, sec, sz, nil, nil); h3 == h1 {
		t.Errorf("hash must change with secret values")
	}
	if h4 := configHash(app, sec, map[string]string{"web": "shared-m"}, nil, nil); h4 == configHash(app, sec, sz, nil, nil) {
		t.Errorf("hash must change with sizes")
	}
	if got := describeSizeChange(map[string]string{"web": "shared-s", "worker": "shared-s"}, map[string]string{"web": "shared-m", "worker": "shared-s"}); got != "Resize web to shared-m" {
		t.Errorf("describeSizeChange = %q", got)
	}
	if len(h1) != 12 {
		t.Errorf("hash length = %d", len(h1))
	}
}

func hasEnv(env []corev1.EnvVar, name, value string) bool {
	for _, e := range env {
		if e.Name == name && e.Value == value {
			return true
		}
	}
	return false
}

func TestReconcileRollbackNote(t *testing.T) {
	imgA := "10.96.0.50:5000/apps/rb@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imgB := "10.96.0.50:5000/apps/rb@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rb", Namespace: "app-rb", Generation: 3,
			Annotations: map[string]string{shpyrdv1.AnnotationReleaseNote: "Rollback to v1"},
		},
		Spec: shpyrdv1.AppSpec{
			Source: &shpyrdv1.Source{Git: &shpyrdv1.GitSource{URL: "https://example.test/r", Revision: "main"}},
			Image:  imgA, // pinned by rollback; source kept
		},
		Status: shpyrdv1.AppStatus{Releases: []shpyrdv1.Release{
			{Number: 1, Image: imgA, Description: "Initial deploy"},
			{Number: 2, Image: imgB, Description: "Deploy"},
		}},
	}
	r, c := newTestReconciler(t, app)
	got := runReconcile(t, r, app)
	if n := len(got.Status.Releases); n != 3 || got.Status.Releases[2].Description != "Rollback to v1" || got.Status.Releases[2].Image != imgA {
		t.Fatalf("releases = %+v", got.Status.Releases)
	}
	if got.Annotations[shpyrdv1.AnnotationReleaseNote] != "" {
		t.Errorf("release note must be cleared after the status is written")
	}
	// A second pass must not add another release nor change the description.
	got = runReconcile(t, r, app)
	if n := len(got.Status.Releases); n != 3 || got.Status.Releases[2].Description != "Rollback to v1" {
		t.Fatalf("second pass changed releases: %+v", got.Status.Releases)
	}
	_ = c
}

func TestRollbackRestoresConfigAndDefaultResources(t *testing.T) {
	img := "10.96.0.50:5000/apps/cfg@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "app-cfg", Generation: 1},
		Spec:       shpyrdv1.AppSpec{Image: img},
	}
	env := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cfg-env", Namespace: "app-cfg"}, Data: map[string][]byte{"GREETING": []byte("v1 value")}}
	r, c := newTestReconciler(t, app, env)

	// v1 with GREETING=v1 value; the release snapshot must exist.
	got := runReconcile(t, r, app)
	if len(got.Status.Releases) != 1 {
		t.Fatalf("releases = %+v", got.Status.Releases)
	}
	snap := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-cfg", Name: "cfg-release-v1"}, snap); err != nil {
		t.Fatalf("snapshot v1 missing: %v", err)
	}
	if string(snap.Data["GREETING"]) != "v1 value" || snap.Labels[shpyrdv1.LabelRelease] != "1" {
		t.Errorf("snapshot content = %v labels=%v", snap.Data, snap.Labels)
	}

	// Default size applied to the container.
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-cfg", Name: "cfg-web"}, d); err != nil {
		t.Fatal(err)
	}
	res := d.Spec.Template.Spec.Containers[0].Resources
	if res.Requests.Cpu().String() != "500m" || res.Limits.Cpu().String() != "2" || res.Limits.Memory().String() != "64Mi" {
		t.Errorf("default (shared-s) resources = %+v", res)
	}
	if ps := got.Status.Processes["web"]; ps.Size != "shared-s" || ps.CPU != "500m" || ps.Memory != "64Mi" {
		t.Errorf("process status size = %+v", ps)
	}

	// Config change -> v2.
	env.Data["GREETING"] = []byte("v2 value")
	if err := c.Update(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, app)
	if len(got.Status.Releases) != 2 {
		t.Fatalf("releases after config change = %+v", got.Status.Releases)
	}

	// Rollback to v1: same build, config must be restored -> v3.
	cur := &shpyrdv1.App{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-cfg", Name: "cfg"}, cur); err != nil {
		t.Fatal(err)
	}
	cur.Annotations = map[string]string{shpyrdv1.AnnotationRollbackTo: "1", shpyrdv1.AnnotationReleaseNote: "Rollback to v1"}
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, app)
	if n := len(got.Status.Releases); n != 3 || got.Status.Releases[2].Description != "Rollback to v1" {
		t.Fatalf("releases after rollback = %+v", got.Status.Releases)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-cfg", Name: "cfg-env"}, env); err != nil {
		t.Fatal(err)
	}
	if string(env.Data["GREETING"]) != "v1 value" {
		t.Errorf("config not restored: %v", env.Data)
	}
	if got.Annotations[shpyrdv1.AnnotationRollbackTo] != "" || got.Annotations[shpyrdv1.AnnotationReleaseNote] != "" {
		t.Errorf("annotations must be consumed: %v", got.Annotations)
	}
	if got.Status.Releases[2].ConfigHash != got.Status.Releases[0].ConfigHash {
		t.Errorf("v3 must carry v1's config hash")
	}
}

func TestDescribeConfigChangeAndSummary(t *testing.T) {
	prev := map[string][]byte{"A": []byte("1"), "B": []byte("2"), "C": []byte("3")}
	cur := map[string][]byte{"A": []byte("1"), "B": []byte("changed"), "D": []byte("4")}
	if got := describeConfigChange(prev, cur); got != "Set B, D, Remove C config vars" {
		t.Errorf("describeConfigChange = %q", got)
	}
	if got := describeConfigChange(prev, prev); got != "Config change" {
		t.Errorf("no diff = %q", got)
	}
	ready, msg := summarizeProcesses(map[string]shpyrdv1.ProcessStatus{
		"web":    {Desired: 3, Ready: 3, Updated: 1},
		"worker": {Desired: 2, Ready: 2, Updated: 2},
	})
	if ready || msg != "web 1/3 updated · worker 2/2" {
		t.Errorf("summary = %v %q", ready, msg)
	}
}

// A registry change (RFC-0059: OCIR replaced by the in-cluster registry)
// recreates the kpack Image, whose tag is immutable.
func TestKpackImageRecreatedWhenRegistryChanges(t *testing.T) {
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: "app-mig", Generation: 1},
		Spec:       shpyrdv1.AppSpec{Source: &shpyrdv1.Source{Git: &shpyrdv1.GitSource{URL: "https://github.com/example/repo", Revision: "main"}}},
	}
	r, c := newTestReconciler(t, app)
	r.Config.RegistryHost = "gru.ocir.io/ns"
	runReconcile(t, r, app)
	img := &unstructured.Unstructured{}
	img.SetGroupVersionKind(KpackImageGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-mig", Name: "mig"}, img); err != nil {
		t.Fatal(err)
	}
	if tag, _, _ := unstructured.NestedString(img.Object, "spec", "tag"); tag != "gru.ocir.io/ns/apps/mig" {
		t.Fatalf("tag = %q", tag)
	}
	firstUID := img.GetUID()

	r.Config.RegistryHost = "10.96.0.50:5000"
	runReconcile(t, r, app)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-mig", Name: "mig"}, img); err != nil {
		t.Fatal(err)
	}
	if tag, _, _ := unstructured.NestedString(img.Object, "spec", "tag"); tag != "10.96.0.50:5000/apps/mig" {
		t.Errorf("tag after registry change = %q", tag)
	}
	if img.GetUID() == firstUID && firstUID != "" {
		t.Error("kpack Image must be recreated, not updated, when the tag changes")
	}
}

// The edge (RFC-0033): a non-public app's Ingress carries the auth_request
// annotations and gets a companion Ingress for /.shpyrd/ plus the server
// alias; a public app has none of it.
func TestEdgeObjects(t *testing.T) {
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "expenses", Namespace: "app-expenses"},
		Spec: shpyrdv1.AppSpec{
			Image:     "ghcr.io/acme/expenses:1",
			Access:    shpyrdv1.AccessAuthenticated,
			Processes: map[string]shpyrdv1.Process{"web": {Port: ptr.To[int32](8080)}},
		},
	}
	r, c := newTestReconciler(t, app)
	r.Config.SystemNamespace = "shpyrd-system"
	runReconcile(t, r, app)

	ing := &networkingv1.Ingress{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-expenses", Name: "expenses"}, ing); err != nil {
		t.Fatal(err)
	}
	if got := ing.Annotations["nginx.ingress.kubernetes.io/auth-url"]; got != "http://shpyrd-server.shpyrd-system.svc.cluster.local/edge/auth?project=expenses&mode=authenticated" {
		t.Errorf("auth-url = %q", got)
	}
	if ing.Annotations["nginx.ingress.kubernetes.io/auth-signin"] == "" || ing.Annotations["nginx.ingress.kubernetes.io/custom-http-errors"] != "403" {
		t.Errorf("edge annotations = %v", ing.Annotations)
	}
	edgeIng := &networkingv1.Ingress{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-expenses", Name: "expenses-edge"}, edgeIng); err != nil {
		t.Fatalf("edge ingress: %v", err)
	}
	if p := edgeIng.Spec.Rules[0].HTTP.Paths[0]; p.Path != "/.shpyrd/" || p.Backend.Service.Name != EdgeServiceName || edgeIng.Annotations["nginx.ingress.kubernetes.io/auth-url"] != "" {
		t.Errorf("edge ingress rule = %+v", p)
	}
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-expenses", Name: EdgeServiceName}, svc); err != nil || svc.Spec.Type != corev1.ServiceTypeExternalName || svc.Spec.ExternalName != "shpyrd-server.shpyrd-system.svc.cluster.local" {
		t.Errorf("edge service: %v %+v", err, svc.Spec)
	}

	// Identified: no sign-in redirect, anonymous passes.
	setAccess := func(access string) {
		t.Helper()
		cur := &shpyrdv1.App{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-expenses", Name: "expenses"}, cur); err != nil {
			t.Fatal(err)
		}
		cur.Spec.Access = access
		if err := c.Update(context.Background(), cur); err != nil {
			t.Fatal(err)
		}
	}
	setAccess(shpyrdv1.AccessIdentified)
	runReconcile(t, r, app)
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-expenses", Name: "expenses"}, ing)
	if ing.Annotations["nginx.ingress.kubernetes.io/auth-signin"] != "" || !strings.Contains(ing.Annotations["nginx.ingress.kubernetes.io/auth-url"], "mode=identified") {
		t.Errorf("identified annotations = %v", ing.Annotations)
	}

	// Public: everything of the edge goes away.
	setAccess(shpyrdv1.AccessPublic)
	runReconcile(t, r, app)
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-expenses", Name: "expenses"}, ing)
	if ing.Annotations["nginx.ingress.kubernetes.io/auth-url"] != "" {
		t.Errorf("public app must carry no auth-url: %v", ing.Annotations)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-expenses", Name: "expenses-edge"}, edgeIng); !kerrors.IsNotFound(err) {
		t.Errorf("edge ingress should be gone: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-expenses", Name: EdgeServiceName}, svc); !kerrors.IsNotFound(err) {
		t.Errorf("edge service should be gone: %v", err)
	}
}

// Allow lists (RFC-0033 phase 5): spec.allow entries inject peer rules
// into the isolation NetworkPolicy.
func TestAllowList(t *testing.T) {
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "crm", Namespace: "app-crm"},
		Spec: shpyrdv1.AppSpec{
			Image:     "ghcr.io/acme/crm:1",
			Processes: map[string]shpyrdv1.Process{"web": {Port: ptr.To[int32](8080)}},
			Allow:     []shpyrdv1.AllowEntry{{Project: "expenses"}, {Platform: "mcp"}},
		},
	}
	r, c := newTestReconciler(t, app)
	r.Config.SystemNamespace = "shpyrd-system"
	runReconcile(t, r, app)

	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-crm", Name: "shpyrd-isolation"}, np); err != nil {
		t.Fatal(err)
	}
	peers := np.Spec.Ingress[0].From
	// base: samePods + nonProject; extras: expenses namespace + shpyrd-server
	if len(peers) < 4 {
		t.Fatalf("ingress peers = %d, want >=4: %+v", len(peers), peers)
	}
	var hasExpenses, hasMCP bool
	for _, p := range peers {
		if p.NamespaceSelector != nil && p.PodSelector != nil {
			if lv, ok := p.NamespaceSelector.MatchLabels[shpyrdv1.LabelProject]; ok && lv == "expenses" {
				hasExpenses = true
			}
			if lv, ok := p.PodSelector.MatchLabels["app.kubernetes.io/name"]; ok && lv == "shpyrd-server" {
				hasMCP = true
			}
		}
	}
	if !hasExpenses || !hasMCP {
		t.Errorf("expenses=%v mcp=%v peers=%+v", hasExpenses, hasMCP, peers)
	}

	// Removing all allows collapses back to the base policy.
	cur := &shpyrdv1.App{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-crm", Name: "crm"}, cur)
	cur.Spec.Allow = nil
	_ = c.Update(context.Background(), cur)
	runReconcile(t, r, cur)
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-crm", Name: "shpyrd-isolation"}, np)
	if len(np.Spec.Ingress[0].From) != 2 {
		t.Errorf("base peers = %d, want 2", len(np.Spec.Ingress[0].From))
	}
}

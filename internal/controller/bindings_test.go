package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// fakeBinder stands in for a resource kind: "ready" resources have vars,
// "slow" ones are still provisioning, others do not exist.
type fakeBinder struct{}

func (fakeBinder) DefaultPrefix() string { return "database" }

func (fakeBinder) ConfigVars(_ context.Context, _ client.Client, ns, name, prefix string) (map[string]string, error) {
	switch name {
	case "db", "db2":
		return map[string]string{prefix + "_URL": "postgres://" + name + "." + ns + ":5432/app", prefix + "_HOST": name}, nil
	case "slow":
		return nil, &NotReadyError{Msg: "Postgres slow is provisioning"}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "shpyrd.io", Resource: "postgres"}, name)
}

func TestBindingsRenderSecretAndRelease(t *testing.T) {
	RegisterBinder("Postgres", fakeBinder{})
	t.Cleanup(func() { delete(binders, "Postgres") })

	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "app-demo", Generation: 1},
		Spec:       shpyrdv1.AppSpec{Image: "ghcr.io/o/demo:1"},
	}
	r, c := newTestReconciler(t, app)
	got := runReconcile(t, r, app)
	markDeploymentReady(t, c, "app-demo", "demo-web", 1)
	got = runReconcile(t, r, got)
	if got.Status.Phase != shpyrdv1.PhaseRunning || len(got.Status.Releases) != 1 {
		t.Fatalf("setup: %s %d releases", got.Status.Phase, len(got.Status.Releases))
	}

	// Attach: a release with the bound vars in its fingerprint.
	got.Spec.Bindings = []shpyrdv1.Binding{{Kind: "Postgres", Name: "db"}}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, got)
	if got.Status.Phase == shpyrdv1.PhaseFailed {
		t.Fatalf("attach failed: %s", got.Status.Message)
	}
	sec := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "demo-bindings"}, sec); err != nil {
		t.Fatalf("bindings secret: %v", err)
	}
	if string(sec.Data["DATABASE_URL"]) != "postgres://db.app-demo:5432/app" || string(sec.Data["DATABASE_HOST"]) != "db" {
		t.Errorf("vars = %v", sec.Data)
	}
	if !strings.Contains(sec.Annotations[shpyrdv1.AnnotationBindingProviders], `"DATABASE_URL":"Postgres/db"`) {
		t.Errorf("providers annotation = %s", sec.Annotations[shpyrdv1.AnnotationBindingProviders])
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != "demo" {
		t.Error("bindings secret must be owned by the app")
	}
	if n := len(got.Status.Releases); n != 2 || got.Status.Releases[1].Description != "Config change" || len(got.Status.Releases[1].Bindings) != 1 {
		t.Errorf("releases = %+v", got.Status.Releases)
	}
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "demo-web"}, d); err != nil {
		t.Fatal(err)
	}
	ef := d.Spec.Template.Spec.Containers[0].EnvFrom
	if len(ef) != 3 || ef[0].SecretRef.Name != "shpyrd-global-env" || ef[1].SecretRef.Name != "demo-env" || ef[2].SecretRef.Name != "demo-bindings" {
		t.Errorf("envFrom order (globals first, bound vars last, so they win) = %+v", ef)
	}

	// Two bindings providing the same var need distinct prefixes.
	got.Spec.Bindings = append(got.Spec.Bindings, shpyrdv1.Binding{Kind: "Postgres", Name: "db2"})
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, got)
	if got.Status.Phase != shpyrdv1.PhaseFailed || !strings.Contains(got.Status.Message, "both provide DATABASE_HOST") {
		t.Errorf("duplicate vars: %s %s", got.Status.Phase, got.Status.Message)
	}
	got.Spec.Bindings[1].Prefix = "analytics"
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, got)
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "demo-bindings"}, sec)
	if got.Status.Phase == shpyrdv1.PhaseFailed || string(sec.Data["ANALYTICS_URL"]) == "" {
		t.Errorf("prefixed binding: %s %v", got.Status.Message, sec.Data)
	}

	// A resource still provisioning makes the app wait (not fail); missing
	// resources and unknown kinds are failures that explain themselves.
	for _, tc := range []struct {
		b     shpyrdv1.Binding
		phase string
		want  string
	}{
		{shpyrdv1.Binding{Kind: "Postgres", Name: "slow"}, shpyrdv1.PhasePending, "waiting for an attached resource: Postgres slow is provisioning"},
		{shpyrdv1.Binding{Kind: "Postgres", Name: "nope"}, shpyrdv1.PhaseFailed, "does not exist in this project"},
		{shpyrdv1.Binding{Kind: "Mongo", Name: "x"}, shpyrdv1.PhaseFailed, `kind "Mongo" cannot be attached (available: Postgres)`},
	} {
		got.Spec.Bindings = []shpyrdv1.Binding{tc.b}
		if err := c.Update(context.Background(), got); err != nil {
			t.Fatal(err)
		}
		got = runReconcile(t, r, got)
		if got.Status.Phase != tc.phase || !strings.Contains(got.Status.Message, tc.want) {
			t.Errorf("%+v: %s %s", tc.b, got.Status.Phase, got.Status.Message)
		}
	}

	// Detach removes the Secret.
	got.Spec.Bindings = nil
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, got)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "demo-bindings"}, sec); !apierrors.IsNotFound(err) {
		t.Errorf("bindings secret should be gone, err=%v", err)
	}
	if got.Status.Phase == shpyrdv1.PhaseFailed {
		t.Errorf("detach: %s", got.Status.Message)
	}
}

func TestEnsureProjectLabel(t *testing.T) {
	app := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "app-demo"}, Spec: shpyrdv1.AppSpec{Image: "x"}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-demo", Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}}}
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-other"}}
	r, c := newTestReconciler(t, app, ns, foreign)
	runReconcile(t, r, app)
	_ = c.Get(context.Background(), types.NamespacedName{Name: "app-demo"}, ns)
	if ns.Labels[shpyrdv1.LabelProject] != "demo" || ns.Labels["pod-security.kubernetes.io/warn"] != "restricted" {
		t.Errorf("namespace labels = %v", ns.Labels)
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: IsolationPolicyName}, np); err != nil {
		t.Fatalf("network policy: %v", err)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 2 || len(np.Spec.Egress) != 1 || len(np.Spec.PolicyTypes) != 2 {
		t.Errorf("policy = %+v", np.Spec)
	}
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "demo-web"}, d); err != nil {
		t.Fatal(err)
	}
	sc := d.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 {
		t.Errorf("hardened container context missing: %+v", sc)
	}
	other := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "app-other"}, Spec: shpyrdv1.AppSpec{Image: "x"}}
	if err := c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r, other)
	_ = c.Get(context.Background(), types.NamespacedName{Name: "app-other"}, foreign)
	if _, ok := foreign.Labels[shpyrdv1.LabelProject]; ok {
		t.Error("namespaces not managed by shpyrd are left alone")
	}
}

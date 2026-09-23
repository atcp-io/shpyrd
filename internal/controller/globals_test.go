package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/configvars"
)

func sampleApp(name string) *shpyrdv1.App {
	return &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "app-" + name, Generation: 1},
		Spec:       shpyrdv1.AppSpec{Processes: map[string]shpyrdv1.Process{"web": {}}},
	}
}

func globalSecret(data map[string]string) *corev1.Secret {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: shpyrdv1.GlobalEnvSecretName, Namespace: "shpyrd-system"}, Type: corev1.SecretTypeOpaque}
	_ = configvars.Apply(sec, data, nil, metav1.Now().Time)
	return sec
}

// RFC-0016: global vars reach every project through a filtered mirror the
// processes read first; a change is a release of its own kind.
func TestGlobalsMirrorAndRelease(t *testing.T) {
	app := sampleApp("web1")
	app.Spec.Image = "registry.test/web1@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r, c := newTestReconciler(t, app, globalSecret(map[string]string{"OPENAI_API_KEY": "sk-1", "REGION": "eu"}))
	got := runReconcile(t, r, app)

	// The mirror carries the data and the per-key metadata, owned by nobody.
	mirror := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: shpyrdv1.GlobalEnvSecretName}, mirror); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if string(mirror.Data["OPENAI_API_KEY"]) != "sk-1" || len(mirror.OwnerReferences) != 0 || len(configvars.List(mirror)) != 2 {
		t.Errorf("mirror = data %v owners %v meta %v", mirror.Data, mirror.OwnerReferences, mirror.Annotations)
	}
	if got.CurrentRelease() == nil || got.CurrentRelease().GlobalHash == "" {
		t.Fatalf("release must record the global fingerprint: %+v", got.Status.Releases)
	}
	first := got.CurrentRelease().Number

	// Changing a global makes a "Global config change" release with a new
	// config hash on the Deployment.
	global := &corev1.Secret{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "shpyrd-system", Name: shpyrdv1.GlobalEnvSecretName}, global)
	_ = configvars.Apply(global, map[string]string{"REGION": "us"}, nil, metav1.Now().Time)
	if err := c.Update(context.Background(), global); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, app)
	cur := got.CurrentRelease()
	if cur.Number != first+1 || cur.Description != "Global config change" {
		t.Fatalf("after global change: %+v", got.Status.Releases)
	}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: shpyrdv1.GlobalEnvSecretName}, mirror)
	if string(mirror.Data["REGION"]) != "us" {
		t.Errorf("mirror not refreshed: %v", mirror.Data)
	}

	// The watch maps the cluster Secret to every App and a mirror to its
	// namespace's App.
	if reqs := r.secretToApps(context.Background(), global); len(reqs) != 1 || reqs[0].Name != "web1" {
		t.Errorf("global secret -> apps = %v", reqs)
	}
	if reqs := r.secretToApps(context.Background(), mirror); len(reqs) != 1 || reqs[0].Namespace != "app-web1" {
		t.Errorf("mirror -> apps = %v", reqs)
	}
	if reqs := r.secretToApps(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "app-web1"}}); reqs != nil {
		t.Errorf("unrelated secret -> %v", reqs)
	}
}

func TestGlobalsOptOut(t *testing.T) {
	app := sampleApp("web1")
	app.Spec.Image = "registry.test/web1@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	app.Spec.Globals = &shpyrdv1.Globals{Exclude: []string{"OPENAI_API_KEY"}}
	r, c := newTestReconciler(t, app, globalSecret(map[string]string{"OPENAI_API_KEY": "sk-1", "REGION": "eu"}))
	runReconcile(t, r, app)
	mirror := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: shpyrdv1.GlobalEnvSecretName}, mirror); err != nil {
		t.Fatal(err)
	}
	if _, has := mirror.Data["OPENAI_API_KEY"]; has || string(mirror.Data["REGION"]) != "eu" || len(configvars.List(mirror)) != 1 {
		t.Errorf("excluded key must not reach the mirror: %v %v", mirror.Data, mirror.Annotations)
	}

	// Disabled: mirror removed, Deployment does not reference it.
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1"}, app)
	app.Spec.Globals = &shpyrdv1.Globals{Disabled: true}
	if err := c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r, app)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: shpyrdv1.GlobalEnvSecretName}, mirror); !apierrors.IsNotFound(err) {
		t.Errorf("mirror should be gone when globals are disabled: %v", err)
	}
	if ef := EnvSources(app); len(ef) != 2 || ef[0].SecretRef.Name != "web1-env" {
		t.Errorf("envFrom with globals disabled = %+v", ef)
	}
}

func TestGlobalHashAndConfigHashIncludeGlobals(t *testing.T) {
	app := sampleApp("x")
	g1 := &corev1.Secret{Data: map[string][]byte{"A": []byte("1")}}
	g2 := &corev1.Secret{Data: map[string][]byte{"A": []byte("2")}}
	if globalHash(nil) != "" || globalHash(g1) == "" || globalHash(g1) == globalHash(g2) {
		t.Error("globalHash")
	}
	if configHash(app, nil, nil, nil, g1) == configHash(app, nil, nil, nil, g2) {
		t.Error("configHash must change with the globals")
	}
	// The same key/value as a binding and as a global are different configs.
	if configHash(app, nil, nil, g1, nil) == configHash(app, nil, nil, nil, g1) {
		t.Error("a binding and a global with the same content must hash differently")
	}
	if configHash(app, nil, nil, nil, g1) != configHash(app, nil, nil, nil, g1) {
		t.Error("configHash must be stable")
	}
}

package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// A private registry (OCIR, ECR...): credentials are mirrored per project,
// builds run as a credentialed ServiceAccount, instances pull with the
// Secret, and BuildKit speaks TLS with its docker config mounted.
func TestPrivateRegistryCredentials(t *testing.T) {
	app := sampleApp("shop")
	app.Spec.Image = "gru.ocir.io/ns/apps/shop@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shpyrd-registry", Namespace: "shpyrd-system"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"gru.ocir.io":{"auth":"eDp5"}}}`)},
	}
	r, c := newTestReconciler(t, app, creds)
	r.Config.RegistryHost = "gru.ocir.io/ns"
	r.Config.RegistrySecret = "shpyrd-registry"
	r.Config.RegistryInsecure = false
	runReconcile(t, r, app)

	// Mirror and build ServiceAccount in the project namespace.
	mirror := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "shpyrd-registry"}, mirror); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if mirror.Type != corev1.SecretTypeDockerConfigJson || string(mirror.Data[corev1.DockerConfigJsonKey]) != string(creds.Data[corev1.DockerConfigJsonKey]) {
		t.Errorf("mirror = %v", mirror)
	}
	sa := &corev1.ServiceAccount{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: BuildServiceAccount}, sa); err != nil {
		t.Fatalf("build SA: %v", err)
	}
	if len(sa.Secrets) != 1 || sa.Secrets[0].Name != "shpyrd-registry" || len(sa.ImagePullSecrets) != 1 {
		t.Errorf("build SA = %+v", sa)
	}
	// Instances pull with the Secret.
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "shop-web"}, dep); err != nil {
		t.Fatal(err)
	}
	if ps := dep.Spec.Template.Spec.ImagePullSecrets; len(ps) != 1 || ps[0].Name != "shpyrd-registry" {
		t.Errorf("imagePullSecrets = %v", ps)
	}
	// The registry Secret event fans out to every App.
	if reqs := r.secretToApps(context.Background(), creds); len(reqs) != 1 || reqs[0].Name != "shop" {
		t.Errorf("registry secret -> apps = %v", reqs)
	}

	// kpack Images build as the credentialed ServiceAccount.
	src := sampleApp("gitapp")
	src.Spec.Source = &shpyrdv1.Source{Git: &shpyrdv1.GitSource{URL: "https://example.test/r.git"}}
	img := r.Config.desiredKpackImage(src)
	if sa, _, _ := unstructured.NestedString(img.Object, "spec", "serviceAccountName"); sa != BuildServiceAccount {
		t.Errorf("kpack serviceAccountName = %q", sa)
	}
	if tag, _, _ := unstructured.NestedString(img.Object, "spec", "tag"); tag != "gru.ocir.io/ns/apps/gitapp" {
		t.Errorf("kpack tag = %q", tag)
	}

	// BuildKit: TLS (no registry.insecure), docker config mounted, pull secret.
	dk := dockerfileApp()
	job := r.Config.desiredBuildJob(dk, 1, "key")
	spec := job.Spec.Template.Spec
	args := strings.Join(spec.Containers[0].Args, " ")
	if strings.Contains(args, "registry.insecure") {
		t.Errorf("TLS registry must not get registry.insecure: %s", args)
	}
	hasCfg := false
	for _, v := range spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "shpyrd-registry" {
			hasCfg = true
		}
	}
	if !hasCfg || len(spec.ImagePullSecrets) != 1 {
		t.Errorf("BuildKit job lacks registry auth: volumes=%v pullSecrets=%v", spec.Volumes, spec.ImagePullSecrets)
	}
	env := spec.Containers[0].Env
	found := false
	for _, e := range env {
		if e.Name == "DOCKER_CONFIG" && e.Value == dockerConfigDir {
			found = true
		}
	}
	if !found {
		t.Errorf("DOCKER_CONFIG not set: %v", env)
	}
	_ = batchv1.JobSpec{}
}

// Without credentials (the in-cluster registry) nothing changes.
func TestNoRegistryCredentials(t *testing.T) {
	app := sampleApp("plain")
	app.Spec.Image = "10.96.0.50:5000/apps/plain@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r, c := newTestReconciler(t, app)
	runReconcile(t, r, app)
	dep := &appsv1.Deployment{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-plain", Name: "plain-web"}, dep)
	if len(dep.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Errorf("no pull secrets expected: %v", dep.Spec.Template.Spec.ImagePullSecrets)
	}
	sa := &corev1.ServiceAccount{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-plain", Name: BuildServiceAccount}, sa); err == nil {
		t.Error("no build SA expected without credentials")
	}
	if r.Config.buildServiceAccountName() != "default" {
		t.Error("kpack must use the default SA without credentials")
	}
}

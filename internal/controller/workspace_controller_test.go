package controller

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/store"
)

func TestWorkspaceFrontDoors(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Slug: "acme", Name: "Acme", Address: "acme.shpyrd.test"}); err != nil {
		t.Fatal(err)
	}
	// Under the platform domain the wildcard covers the dashboard host.
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Slug: "beta", Name: "Beta", Address: "beta.example.test"}); err != nil {
		t.Fatal(err)
	}
	// No address yet: nothing to publish.
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Slug: "pending", Name: "Pending"}); err != nil {
		t.Fatal(err)
	}
	base, c := newTestReconciler(t)
	r := &WorkspaceReconciler{Client: c, Scheme: base.Scheme, Store: st, Config: Config{
		Domain: "example.test", SystemNamespace: "shpyrd-system", ClusterIssuer: "letsencrypt", IngressClassExternal: "nginx", WildcardTLS: true,
	}}
	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// acme: its own host, its own certificate, on the external front door.
	ing := &networkingv1.Ingress{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "shpyrd-system", Name: "workspace-acme"}, ing); err != nil {
		t.Fatalf("acme ingress: %v", err)
	}
	if ing.Spec.Rules[0].Host != "acme.shpyrd.test" || *ing.Spec.IngressClassName != "nginx" || ing.Spec.TLS[0].SecretName != "workspace-acme-tls" {
		t.Errorf("acme ingress = %+v", ing.Spec)
	}
	if svc := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service; svc.Name != "shpyrd-server" || svc.Port.Name != "http" {
		t.Errorf("acme backend = %+v", svc)
	}
	if ing.Labels[shpyrdv1.LabelWorkspace] != "acme" {
		t.Errorf("acme labels = %v", ing.Labels)
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(CertificateGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "shpyrd-system", Name: "workspace-acme-tls"}, cert); err != nil {
		t.Fatalf("acme certificate: %v", err)
	}
	names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	if len(names) != 1 || names[0] != "acme.shpyrd.test" {
		t.Errorf("acme certificate names = %v", names)
	}

	// beta: covered by the platform wildcard, no certificate of its own.
	if err := c.Get(ctx, types.NamespacedName{Namespace: "shpyrd-system", Name: "workspace-beta"}, ing); err != nil {
		t.Fatalf("beta ingress: %v", err)
	}
	if ing.Spec.TLS[0].SecretName != "" {
		t.Errorf("beta must use the wildcard: %+v", ing.Spec.TLS)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "shpyrd-system", Name: "workspace-beta-tls"}, cert); err == nil {
		t.Error("beta got a certificate although the wildcard covers it")
	}
	// pending: nothing.
	if err := c.Get(ctx, types.NamespacedName{Namespace: "shpyrd-system", Name: "workspace-pending"}, ing); err == nil {
		t.Error("a workspace without address got a front door")
	}

	// A workspace that disappears from the store loses its front door.
	m2 := store.NewMemory()
	if _, err := m2.CreateWorkspace(ctx, store.Workspace{Slug: "beta", Name: "Beta", Address: "beta.example.test"}); err != nil {
		t.Fatal(err)
	}
	r.Store = m2
	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "shpyrd-system", Name: "workspace-acme"}, ing); err == nil {
		t.Error("acme's front door survived its workspace")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "shpyrd-system", Name: "workspace-beta"}, ing); err != nil {
		t.Errorf("beta's front door must stay: %v", err)
	}
}

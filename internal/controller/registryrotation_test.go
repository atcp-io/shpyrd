package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// RFC-0059: a renewed registry certificate restarts the registry; the
// first pass only records the running one.
func TestRegistryRotationRestartsOnRenewal(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-tls", Namespace: "shpyrd-system"}, Data: map[string][]byte{corev1.TLSCertKey: []byte("cert-1")}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: "shpyrd-system"}}
	c := fake.NewClientBuilder().WithObjects(sec, dep).Build()
	r := &RegistryRotation{Client: c, Namespace: "shpyrd-system", Secret: "registry-tls", Deploy: "registry"}
	ctx := context.Background()

	restarted, err := r.Check(ctx)
	if err != nil || restarted {
		t.Fatalf("first pass stamps only: restarted=%v err=%v", restarted, err)
	}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "shpyrd-system", Name: "registry"}, dep)
	first := dep.Spec.Template.Annotations[AnnotationTLSChecksum]
	if first == "" {
		t.Fatal("no checksum stamped")
	}
	if restarted, err := r.Check(ctx); err != nil || restarted {
		t.Fatalf("same certificate: restarted=%v err=%v", restarted, err)
	}

	sec.Data[corev1.TLSCertKey] = []byte("cert-2")
	if err := c.Update(ctx, sec); err != nil {
		t.Fatal(err)
	}
	restarted, err = r.Check(ctx)
	if err != nil || !restarted {
		t.Fatalf("renewed certificate must restart: restarted=%v err=%v", restarted, err)
	}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "shpyrd-system", Name: "registry"}, dep)
	if dep.Spec.Template.Annotations[AnnotationTLSChecksum] == first {
		t.Error("checksum not updated")
	}
}

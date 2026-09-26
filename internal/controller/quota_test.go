package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/project"
	"shpyrd/pkg/store"
)

func TestReconcileQuota(t *testing.T) {
	ctx := context.Background()
	acme := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "app-acme-shop", Labels: project.NamespaceLabels("acme", "shop")}}
	free := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	r, c := newTestReconciler(t, acme, free)
	plans := map[string]*store.Limits{"acme": {CPU: "4", Memory: "8Gi", Storage: "50Gi", Instances: 10}}
	r.Config.WorkspaceLimits = func(slug string) *store.Limits { return plans[slug] }

	if err := r.reconcileQuota(ctx, acme); err != nil {
		t.Fatal(err)
	}
	q := &corev1.ResourceQuota{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "app-acme-shop", Name: QuotaName}, q); err != nil {
		t.Fatalf("quota: %v", err)
	}
	for res, want := range map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "4", corev1.ResourceRequestsMemory: "8Gi", corev1.ResourceRequestsStorage: "50Gi"} {
		if got := q.Spec.Hard[res]; got.Cmp(mustQ(want)) != 0 {
			t.Errorf("hard %s = %s, want %s", res, got.String(), want)
		}
	}
	if _, ok := q.Spec.Hard[corev1.ResourcePods]; ok {
		t.Error("pods must not be capped: builds and one-off commands would count")
	}
	// The implicit workspace has no plan: no quota.
	if err := r.reconcileQuota(ctx, free); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "app-blog", Name: QuotaName}, q); err == nil {
		t.Error("a workspace without a plan got a quota")
	}
	// A plan that goes away takes its quota along.
	delete(plans, "acme")
	if err := r.reconcileQuota(ctx, acme); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "app-acme-shop", Name: QuotaName}, q); err == nil {
		t.Error("quota survived its plan")
	}
}

func mustQ(s string) resource.Quantity { return resource.MustParse(s) }

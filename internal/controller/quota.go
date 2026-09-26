package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/store"
)

// QuotaName is the ResourceQuota the platform keeps in project namespaces
// of workspaces with a plan (RFC-0033 phase 6, RFC-0042).
const QuotaName = "shpyrd"

// reconcileQuota backs the API's plan check with a ResourceQuota: the
// plan's ceilings on requests and storage, per project namespace. A
// namespace cannot hold more than the whole plan; the API keeps the sum
// across projects under it. Workspaces without a plan get no quota, and a
// quota left behind by a plan that went away is removed.
func (r *AppReconciler) reconcileQuota(ctx context.Context, app *shpyrdv1.App) error {
	var limits *store.Limits
	if r.Config.WorkspaceLimits != nil {
		limits = r.Config.WorkspaceLimits(workspaceOf(app))
	}
	q := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: QuotaName, Namespace: app.Namespace}}
	hard := quotaHard(limits)
	if len(hard) == 0 {
		if err := r.Delete(ctx, q); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("remove quota: %w", err)
		}
		return nil
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, q, func() error {
		q.Labels = mergeMaps(q.Labels, commonLabels(app))
		q.Spec.Hard = hard
		return nil
	})
	if err != nil {
		return fmt.Errorf("quota: %w", err)
	}
	return nil
}

// quotaHard renders a plan as ResourceQuota ceilings; nil when the plan
// caps nothing Kubernetes can enforce. Instances and projects are the
// API's to count: pods would also count builds and one-off commands.
func quotaHard(l *store.Limits) corev1.ResourceList {
	if l == nil {
		return nil
	}
	hard := corev1.ResourceList{}
	if q, err := resource.ParseQuantity(l.CPU); err == nil && l.CPU != "" {
		hard[corev1.ResourceRequestsCPU] = q
	}
	if q, err := resource.ParseQuantity(l.Memory); err == nil && l.Memory != "" {
		hard[corev1.ResourceRequestsMemory] = q
	}
	if q, err := resource.ParseQuantity(l.Storage); err == nil && l.Storage != "" {
		hard[corev1.ResourceRequestsStorage] = q
	}
	if len(hard) == 0 {
		return nil
	}
	return hard
}

var _ client.Object = &corev1.ResourceQuota{}

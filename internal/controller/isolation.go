package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// Project isolation (RFC-0008): a NetworkPolicy per project namespace and
// Pod Security Standard labels. Ingress is allowed from the project's own
// pods, the ingress controller (the URL) and the monitoring namespace
// (metrics); everything else, including other projects, is refused. Egress
// may reach the project's pods, every non-project namespace (DNS, the
// registry, bound platform services) and the internet; when the pod CIDR is
// known, pods of other projects are excluded from the internet rule too.

// IsolationPolicyName is the NetworkPolicy in every project namespace.
const IsolationPolicyName = "shpyrd-isolation"

// Pod Security Standards are applied in warn and audit mode: the app
// containers are hardened by the controller, build pods (BuildKit) need
// exemptions an enforcing mode would not give.
var podSecurityLabels = map[string]string{
	"pod-security.kubernetes.io/warn":  "restricted",
	"pod-security.kubernetes.io/audit": "restricted",
}

// ensureNamespaceLabels marks the namespace as this project and applies the
// pod security labels; older namespaces were created without them.
func (r *AppReconciler) ensureNamespaceLabels(ctx context.Context, app *shpyrdv1.App) error {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: app.Namespace}, ns); err != nil {
		return client.IgnoreNotFound(err)
	}
	if ns.Labels[shpyrdv1.LabelManagedBy] != "shpyrd" {
		return nil
	}
	want := map[string]string{shpyrdv1.LabelProject: app.Name}
	for k, v := range podSecurityLabels {
		want[k] = v
	}
	changed := false
	for k, v := range want {
		if ns.Labels[k] != v {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	patch := client.MergeFrom(ns.DeepCopy())
	ns.Labels = mergeMaps(ns.Labels, want)
	return r.Patch(ctx, ns, patch)
}

// reconcileIsolation keeps the project's NetworkPolicy in place.
func (r *AppReconciler) reconcileIsolation(ctx context.Context, app *shpyrdv1.App) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: IsolationPolicyName, Namespace: app.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = mergeMaps(np.Labels, map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"})
		np.Spec = r.Config.isolationPolicy()
		return nil
	})
	if err != nil {
		return fmt.Errorf("network policy: %w", err)
	}
	return nil
}

func namespaceNamed(name string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": name}}}
}

// isolationPolicy is the spec applied to every project namespace.
func (c Config) isolationPolicy() networkingv1.NetworkPolicySpec {
	samePods := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}
	spec := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{samePods, namespaceNamed(c.IngressNamespace), namespaceNamed(c.MonitoringNamespace)},
		}},
	}
	// Egress: own pods, platform namespaces (anything that is not a project),
	// and the internet.
	nonProject := networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: shpyrdv1.LabelProject, Operator: metav1.LabelSelectorOpDoesNotExist}},
	}}
	internet := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}
	if c.PodCIDR != "" {
		internet.IPBlock.Except = []string{c.PodCIDR}
	}
	spec.Egress = []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{samePods, nonProject, internet}}}
	return spec
}

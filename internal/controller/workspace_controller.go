package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/store"
)

// WorkspaceReconciler gives every explicit workspace (RFC-0033 phase 6) a
// front door: an Ingress at its address pointing at the platform server,
// with a certificate, so its dashboard, sign-in and edge answer there. The
// implicit workspace has the platform's own Ingress; on the open-source
// platform this reconciler therefore does nothing. Workspaces come from
// the control-plane store, not from Kubernetes objects, so the pass runs
// on Notify and on a timer.
type WorkspaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Store  store.Store
	Config Config

	events chan event.GenericEvent
}

const workspaceSync = 2 * time.Minute

var workspacesRequest = reconcile.Request{NamespacedName: client.ObjectKey{Name: "workspaces"}}

func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.events = make(chan event.GenericEvent, 1)
	return ctrl.NewControllerManagedBy(mgr).
		Named("workspaces").
		WatchesRawSource(source.Channel(r.events, handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{workspacesRequest}
		}))).
		Complete(r)
}

// Notify asks for a pass now (after a workspace was created or changed).
func (r *WorkspaceReconciler) Notify() {
	if r.events == nil {
		return
	}
	select {
	case r.events <- event.GenericEvent{Object: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workspaces"}}}:
	default:
	}
}

// Start kicks the first pass once the manager runs (the store has no
// watch to trigger it).
func (r *WorkspaceReconciler) Start(ctx context.Context) error {
	r.Notify()
	<-ctx.Done()
	return nil
}

func (r *WorkspaceReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	all, err := r.Store.ListWorkspaces(ctx)
	if err != nil {
		return ctrl.Result{RequeueAfter: workspaceSync}, err
	}
	wanted := map[string]bool{}
	for i := range all {
		ws := &all[i]
		if ws.Implicit() || ws.Address == "" {
			continue
		}
		wanted[ws.Slug] = true
		if err := r.ensureFrontDoor(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Front doors of workspaces that are gone.
	var list networkingv1.IngressList
	if err := r.List(ctx, &list, client.InNamespace(r.Config.SystemNamespace), client.MatchingLabels{shpyrdv1.LabelManagedBy: "shpyrd", "shpyrd.io/workspace-front-door": "true"}); err != nil {
		return ctrl.Result{}, err
	}
	for i := range list.Items {
		ing := &list.Items[i]
		if slug := ing.Labels[shpyrdv1.LabelWorkspace]; !wanted[slug] {
			if err := r.Delete(ctx, ing); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			cert := &unstructured.Unstructured{}
			cert.SetGroupVersionKind(CertificateGVK)
			cert.SetName(ing.Name + "-tls")
			cert.SetNamespace(ing.Namespace)
			if err := r.Delete(ctx, cert); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
				return ctrl.Result{}, err
			}
			logger.Info("workspace front door removed", "workspace", slug)
		}
	}
	return ctrl.Result{RequeueAfter: workspaceSync}, nil
}

// workspaceFrontDoorName names the Ingress and Certificate of a workspace.
func workspaceFrontDoorName(slug string) string { return "workspace-" + slug }

// ensureFrontDoor keeps the Ingress (and certificate) of one workspace.
// Tenants reach their dashboard from the internet, so the external front
// door serves it whatever the platform's own exposure.
func (r *WorkspaceReconciler) ensureFrontDoor(ctx context.Context, ws *store.Workspace) error {
	name := workspaceFrontDoorName(ws.Slug)
	labels := map[string]string{
		shpyrdv1.LabelManagedBy:          "shpyrd",
		shpyrdv1.LabelWorkspace:          ws.Slug,
		"shpyrd.io/workspace-front-door": "true",
	}
	wildcard := r.Config.WildcardTLS && r.Config.underClusterDomain(ws.Address)
	tls := networkingv1.IngressTLS{Hosts: []string{ws.Address}}
	if !wildcard {
		tls.SecretName = name + "-tls"
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(CertificateGVK)
		cert.SetName(name + "-tls")
		cert.SetNamespace(r.Config.SystemNamespace)
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
			cert.SetLabels(mergeMaps(cert.GetLabels(), labels))
			_ = unstructured.SetNestedField(cert.Object, name+"-tls", "spec", "secretName")
			_ = unstructured.SetNestedStringSlice(cert.Object, []string{ws.Address}, "spec", "dnsNames")
			_ = unstructured.SetNestedMap(cert.Object, map[string]interface{}{"kind": "ClusterIssuer", "name": r.Config.ClusterIssuer}, "spec", "issuerRef")
			return nil
		}); err != nil {
			return fmt.Errorf("certificate for workspace %s: %w", ws.Slug, err)
		}
	}
	class := r.Config.IngressClassExternal
	if class == "" {
		class = "nginx"
	}
	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Config.SystemNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = mergeMaps(ing.Labels, labels)
		ing.Annotations = mergeMaps(ing.Annotations, map[string]string{"nginx.ingress.kubernetes.io/ssl-redirect": "true"})
		ing.Spec.IngressClassName = &class
		ing.Spec.TLS = []networkingv1.IngressTLS{tls}
		ing.Spec.Rules = []networkingv1.IngressRule{{
			Host: ws.Address,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/", PathType: &pathType,
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "shpyrd-server", Port: networkingv1.ServiceBackendPort{Name: "http"}}},
			}}}},
		}}
		return nil
	})
	if err != nil {
		return fmt.Errorf("front door for workspace %s: %w", ws.Slug, err)
	}
	return nil
}

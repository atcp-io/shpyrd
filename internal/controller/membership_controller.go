package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/authz"
)

// MembershipReconciler mirrors Teams and ProjectMembers into Kubernetes RBAC
// (RFC-0008): per project namespace a RoleBinding per role to the users and
// groups holding it, and ClusterRoleBindings for the platform roles. The
// whole mirror is recomputed on any change: it is small and the outcome is
// deterministic, so no per-object bookkeeping is needed.
type MembershipReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// ClusterRoles the mirror binds; they ship with the shpyrd component.
var projectClusterRoles = map[string]string{
	shpyrdv1.RoleViewer:    "shpyrd-project-viewer",
	shpyrdv1.RoleDeveloper: "shpyrd-project-developer",
	shpyrdv1.RoleAdmin:     "shpyrd-project-admin",
}

var platformClusterRoles = map[string][]string{
	shpyrdv1.RolePlatformViewer: {"shpyrd-platform-viewer", "shpyrd-project-viewer"},
	shpyrdv1.RolePlatformAdmin:  {"shpyrd-platform-admin", "shpyrd-platform-viewer", "shpyrd-project-viewer", "shpyrd-project-developer", "shpyrd-project-admin"},
}

// mirrorRequest is the single reconcile key: every event maps to it.
var mirrorRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "memberships"}}

func toMirror(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{mirrorRequest}
}

var isProjectNamespace = predicate.NewPredicateFuncs(func(o client.Object) bool {
	_, ok := o.GetLabels()[shpyrdv1.LabelProject]
	return ok
})

func (r *MembershipReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("memberships").
		Watches(&shpyrdv1.Team{}, handler.EnqueueRequestsFromMapFunc(toMirror)).
		Watches(&shpyrdv1.ProjectMember{}, handler.EnqueueRequestsFromMapFunc(toMirror)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(toMirror), builder.WithPredicates(isProjectNamespace)).
		Complete(r)
}

func (r *MembershipReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	snap, err := authz.Load(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces, client.HasLabels{shpyrdv1.LabelProject}); err != nil {
		return ctrl.Result{}, err
	}
	for _, ns := range namespaces.Items {
		if ns.DeletionTimestamp != nil {
			continue
		}
		project := ns.Labels[shpyrdv1.LabelProject]
		for role, clusterRole := range projectClusterRoles {
			subjects := projectSubjects(snap, project, role)
			if err := r.ensureRoleBinding(ctx, ns.Name, "shpyrd-"+role, clusterRole, subjects); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	for role, clusterRoles := range platformClusterRoles {
		subjects := platformSubjects(snap, role)
		for _, cr := range clusterRoles {
			name := "shpyrd-" + role + "-" + strings.TrimPrefix(cr, "shpyrd-")
			if err := r.ensureClusterRoleBinding(ctx, name, cr, subjects); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	logger.V(1).Info("rbac mirror reconciled", "namespaces", len(namespaces.Items), "teams", len(snap.Teams), "members", len(snap.Members))
	// Namespaces created later are caught by the watch; a periodic pass
	// covers anything else.
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

// projectSubjects lists the users and groups holding exactly role on project.
func projectSubjects(snap *authz.Snapshot, project, role string) []rbacv1.Subject {
	set := subjectSet{}
	for _, m := range snap.Members {
		if m.Spec.Project != project || m.Spec.Role != role {
			continue
		}
		if m.Spec.User != "" {
			set.user(m.Spec.User)
		}
		if m.Spec.Team != "" {
			set.team(snap, m.Spec.Team)
		}
	}
	return set.sorted()
}

// platformSubjects lists the members of teams holding a platform role.
func platformSubjects(snap *authz.Snapshot, role string) []rbacv1.Subject {
	set := subjectSet{}
	for _, t := range snap.Teams {
		if t.Spec.PlatformRole == role {
			set.team(snap, t.Name)
		}
	}
	return set.sorted()
}

type subjectSet map[string]rbacv1.Subject

func (s subjectSet) user(email string) {
	email = strings.ToLower(email)
	s["u:"+email] = rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: email}
}

func (s subjectSet) group(name string) {
	s["g:"+name] = rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: name}
}

func (s subjectSet) team(snap *authz.Snapshot, name string) {
	for _, t := range snap.Teams {
		if t.Name != name {
			continue
		}
		for _, m := range t.Spec.Members {
			s.user(m)
		}
		for _, g := range t.Spec.Groups {
			s.group(g)
		}
	}
}

func (s subjectSet) sorted() []rbacv1.Subject {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]rbacv1.Subject, 0, len(keys))
	for _, k := range keys {
		out = append(out, s[k])
	}
	return out
}

// ensureRoleBinding creates/updates the binding, or deletes it when nobody
// holds the role.
func (r *MembershipReconciler) ensureRoleBinding(ctx context.Context, namespace, name, clusterRole string, subjects []rbacv1.Subject) error {
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if len(subjects) == 0 {
		if err := r.Delete(ctx, rb); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete rolebinding %s/%s: %w", namespace, name, err)
		}
		return nil
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Labels = mergeMaps(rb.Labels, map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"})
		rb.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole}
		rb.Subjects = subjects
		return nil
	})
	if err != nil {
		return fmt.Errorf("rolebinding %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (r *MembershipReconciler) ensureClusterRoleBinding(ctx context.Context, name, clusterRole string, subjects []rbacv1.Subject) error {
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if len(subjects) == 0 {
		if err := r.Delete(ctx, crb); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete clusterrolebinding %s: %w", name, err)
		}
		return nil
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, crb, func() error {
		crb.Labels = mergeMaps(crb.Labels, map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"})
		crb.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole}
		crb.Subjects = subjects
		return nil
	})
	if err != nil {
		return fmt.Errorf("clusterrolebinding %s: %w", name, err)
	}
	return nil
}

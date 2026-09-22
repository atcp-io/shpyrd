package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

func TestMembershipMirror(t *testing.T) {
	shopNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-shop", Labels: map[string]string{shpyrdv1.LabelProject: "shop", shpyrdv1.LabelManagedBy: "shpyrd"}}}
	blogNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-blog", Labels: map[string]string{shpyrdv1.LabelProject: "blog"}}}
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}
	web := &shpyrdv1.Team{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: shpyrdv1.TeamSpec{Members: []string{"Ada@Example.test", "bob@example.test"}, Groups: []string{"engineering"}}}
	ops := &shpyrdv1.Team{ObjectMeta: metav1.ObjectMeta{Name: "ops"}, Spec: shpyrdv1.TeamSpec{Members: []string{"ops@example.test"}, PlatformRole: shpyrdv1.RolePlatformAdmin}}
	devs := &shpyrdv1.ProjectMember{ObjectMeta: metav1.ObjectMeta{Name: "shop-dev"}, Spec: shpyrdv1.ProjectMemberSpec{Project: "shop", Role: shpyrdv1.RoleDeveloper, Team: "web"}}
	guest := &shpyrdv1.ProjectMember{ObjectMeta: metav1.ObjectMeta{Name: "shop-guest"}, Spec: shpyrdv1.ProjectMemberSpec{Project: "shop", Role: shpyrdv1.RoleViewer, User: "guest@example.test"}}
	base, c := newTestReconciler(t, shopNS, blogNS, other, web, ops, devs, guest)
	r := &MembershipReconciler{Client: c, Scheme: base.Scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rb := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "shpyrd-developer"}, rb); err != nil {
		t.Fatalf("developer binding: %v", err)
	}
	if rb.RoleRef.Name != "shpyrd-project-developer" || len(rb.Subjects) != 3 {
		t.Fatalf("developer binding = %+v", rb)
	}
	// Sorted: groups after users; emails lower-cased.
	if rb.Subjects[0].Kind != "Group" || rb.Subjects[0].Name != "engineering" || rb.Subjects[1].Name != "ada@example.test" || rb.Subjects[2].Name != "bob@example.test" {
		t.Errorf("subjects = %+v", rb.Subjects)
	}
	viewer := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "shpyrd-viewer"}, viewer); err != nil || len(viewer.Subjects) != 1 || viewer.Subjects[0].Name != "guest@example.test" {
		t.Errorf("viewer binding: %v %+v", err, viewer.Subjects)
	}
	// No admins on shop, nobody on blog: no bindings there.
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "shpyrd-admin"}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("admin binding should not exist: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-blog", Name: "shpyrd-developer"}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("blog binding should not exist: %v", err)
	}
	// Platform admins are bound cluster-wide.
	crb := &rbacv1.ClusterRoleBinding{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "shpyrd-platform-admin-project-admin"}, crb); err != nil || len(crb.Subjects) != 1 || crb.Subjects[0].Name != "ops@example.test" {
		t.Errorf("platform binding: %v %+v", err, crb.Subjects)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "shpyrd-platform-viewer-project-viewer"}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("no platform viewers: %v", err)
	}

	// Removing the grant removes the binding.
	if err := c.Delete(context.Background(), devs); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "shpyrd-developer"}, rb); !apierrors.IsNotFound(err) {
		t.Errorf("developer binding should be gone: %v", err)
	}
	var list rbacv1.RoleBindingList
	_ = c.List(context.Background(), &list, client.InNamespace("kube-system"))
	if len(list.Items) != 0 {
		t.Error("only project namespaces get bindings")
	}
}

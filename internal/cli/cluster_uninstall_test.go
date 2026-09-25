package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
)

// destroy on a cloud context: projects first, then load balancers, then the
// platform's claims (after stopping what holds them), with a warning for
// retained volumes and the infrastructure command at the end.
func TestDestroyCloudOrderAndOutput(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	app := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "app-shop"}}
	cr := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()

	fifty := resource.MustParse("50Gi")
	objs := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-shop"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shpyrd-system"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "ingress-nginx-controller", Namespace: "ingress-nginx"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "shpyrd-server", Namespace: "shpyrd-system"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "registry-data", Namespace: "shpyrd-system"}, Status: corev1.PersistentVolumeClaimStatus{Capacity: corev1.ResourceList{corev1.ResourceStorage: fifty}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: "shpyrd-system"}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "registry-x", Namespace: "shpyrd-system"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "registry-data"}}}}}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "kept"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, Capacity: corev1.ResourceList{corev1.ResourceStorage: fifty}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: install.InstallRecordName, Namespace: install.DefaultSystemNamespace}},
	}
	cs := kubefake.NewSimpleClientset(objs...)
	k := &kube.Client{Kube: cs, Namespace: "shpyrd-system"}
	info := &install.InstallInfo{Profile: "oci", Version: "v0.1.12", Vars: map[string]string{install.VarCluster: "shpyrd-dev"}}
	ctx := context.Background()

	// Refused: nothing happens.
	var out bytes.Buffer
	if err := runDestroyCloud(ctx, &out, k, cr, info, "oke-shpyrd-dev", func(string) bool { return false }); err == nil || err.Error() != "aborted" {
		t.Fatalf("expected abort, got %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, "app-shop", metav1.GetOptions{}); err != nil {
		t.Fatalf("project deleted despite refusal: %v", err)
	}
	if !strings.Contains(out.String(), "projects:       1 (shop)") || !strings.Contains(out.String(), "load balancers: 1 (ingress-nginx/ingress-nginx-controller)") || !strings.Contains(out.String(), "disks:          1 claim(s), 50 GiB") {
		t.Errorf("summary:\n%s", out.String())
	}

	out.Reset()
	if err := runDestroyCloud(ctx, &out, k, cr, info, "oke-shpyrd-dev", func(string) bool { return true }); err != nil {
		t.Fatalf("destroy: %v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{"Deleting 1 project(s)", "projects: done", "Deleting 1 load balancer(s)", "load balancers: done", "Deleting 1 disk(s)", "disks: done", "Retain policy", "kept (50Gi)", "terraform destroy", "kubectl config delete-context oke-shpyrd-dev"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if i, j := strings.Index(text, "projects: done"), strings.Index(text, "Deleting 1 load balancer"); i > j {
		t.Errorf("load balancers must go after projects")
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, "app-shop", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("project namespace still there: %v", err)
	}
	if _, err := cs.CoreV1().Services("ingress-nginx").Get(ctx, "ingress-nginx-controller", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("load balancer service still there: %v", err)
	}
	if _, err := cs.CoreV1().Services("shpyrd-system").Get(ctx, "shpyrd-server", metav1.GetOptions{}); err != nil {
		t.Errorf("ClusterIP service must stay: %v", err)
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims("shpyrd-system").Get(ctx, "registry-data", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("claim still there: %v", err)
	}
	if d, err := cs.AppsV1().Deployments("shpyrd-system").Get(ctx, "registry", metav1.GetOptions{}); err != nil || *d.Spec.Replicas != 0 {
		t.Errorf("registry not scaled down: %v %+v", err, d.Spec.Replicas)
	}
	if _, err := cs.CoreV1().ConfigMaps(install.DefaultSystemNamespace).Get(ctx, install.InstallRecordName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("install record still there: %v", err)
	}
	var _ client.Client = cr
}

func TestIsKindContext(t *testing.T) {
	if !isKindContext("kind-shpyrd") || isKindContext("oke-shpyrd-dev") || isKindContext("") {
		t.Error("kind contexts start with kind-")
	}
}

// --vars-file: the infrastructure's values load first; --domain and --set
// win over them.
func TestVarsFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.vars"
	if err := os.WriteFile(path, []byte("# from terraform\nSHPYRD_DOMAIN=aws.example.com\nSHPYRD_EFS_ID=\"fs-1\"\nSHPYRD_LB_IP=1.2.3.4,5.6.7.8\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &initFlags{domain: defaultDomain, varsFile: path, set: []string{"SHPYRD_EFS_ID=fs-2"}}
	vars, err := f.vars("dev")
	if err != nil {
		t.Fatal(err)
	}
	if vars[install.VarDomain] != "aws.example.com" || vars[install.VarEFSID] != "fs-2" || vars[install.VarLBIP] != "1.2.3.4,5.6.7.8" {
		t.Errorf("vars = %v", vars)
	}
	f.domainExplicit, f.domain = true, "other.example.com"
	vars, _ = f.vars("dev")
	if vars[install.VarDomain] != "other.example.com" {
		t.Errorf("explicit --domain must win: %v", vars[install.VarDomain])
	}
	if err := os.WriteFile(path, []byte("DOMAIN=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.vars("dev"); err == nil || !strings.Contains(err.Error(), "SHPYRD_NAME=value") {
		t.Errorf("bad line must be refused: %v", err)
	}
}

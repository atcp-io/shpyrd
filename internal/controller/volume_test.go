package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/kube"
)

func newVolumeReconciler(t *testing.T, objs ...client.Object) (*VolumeReconciler, client.Client) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&shpyrdv1.Volume{}, &corev1.PersistentVolumeClaim{}).Build()
	return &VolumeReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(50)}, c
}

func reconcileVolume(t *testing.T, r *VolumeReconciler, vol *shpyrdv1.Volume) *shpyrdv1.Volume {
	t.Helper()
	key := types.NamespacedName{Namespace: vol.Namespace, Name: vol.Name}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	out := &shpyrdv1.Volume{}
	if err := r.Get(context.Background(), key, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func testVolume(name, size string, mode corev1.PersistentVolumeAccessMode) *shpyrdv1.Volume {
	return &shpyrdv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "app-demo", Generation: 1},
		Spec:       shpyrdv1.VolumeSpec{Size: resource.MustParse(size), AccessMode: mode},
	}
}

func TestVolumeCreatesClaimAndTracksMounts(t *testing.T) {
	vol := testVolume("data", "5Gi", "")
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "app-demo"},
		Spec: shpyrdv1.AppSpec{Processes: map[string]shpyrdv1.Process{
			"web": {Volumes: []shpyrdv1.VolumeMount{{Name: "data", Path: "/data"}}},
		}},
	}
	r, c := newVolumeReconciler(t, vol, app)

	got := reconcileVolume(t, r, vol)
	if got.Status.Phase != shpyrdv1.VolumePending || len(got.Status.MountedBy) != 1 || got.Status.MountedBy[0] != "demo/web" {
		t.Fatalf("status = %+v", got.Status)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "vol-data"}, pvc); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce || pvc.Spec.Resources.Requests.Storage().String() != "5Gi" || pvc.Spec.StorageClassName != nil {
		t.Errorf("claim spec = %+v", pvc.Spec)
	}
	if len(pvc.OwnerReferences) != 1 || pvc.OwnerReferences[0].Kind != "Volume" {
		t.Error("claim must be owned by the Volume")
	}

	// Bound with capacity.
	pvc.Status.Phase = corev1.ClaimBound
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}
	if err := c.Status().Update(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	got = reconcileVolume(t, r, vol)
	if got.Status.Phase != shpyrdv1.VolumeBound || got.Status.Capacity != "5Gi" {
		t.Fatalf("status = %+v", got.Status)
	}

	// Mapping App -> Volumes.
	if reqs := appToVolumes(context.Background(), app); len(reqs) != 1 || reqs[0].Name != "data" {
		t.Errorf("appToVolumes = %+v", reqs)
	}
}

func TestVolumeResizeRules(t *testing.T) {
	vol := testVolume("data", "5Gi", "")
	expandable := &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: "standard", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}},
		Provisioner:          "x",
		AllowVolumeExpansion: ptr.To(true),
	}
	r, c := newVolumeReconciler(t, vol, expandable)
	reconcileVolume(t, r, vol)

	// Grow: allowed by the default class.
	vol = reconcileVolume(t, r, vol)
	vol.Spec.Size = resource.MustParse("10Gi")
	if err := c.Update(context.Background(), vol); err != nil {
		t.Fatal(err)
	}
	got := reconcileVolume(t, r, vol)
	if got.Status.Phase == shpyrdv1.VolumeFailed {
		t.Fatalf("grow refused: %s", got.Status.Message)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "vol-data"}, pvc)
	if pvc.Spec.Resources.Requests.Storage().String() != "10Gi" {
		t.Errorf("claim not expanded: %s", pvc.Spec.Resources.Requests.Storage())
	}

	// Shrink: refused.
	got.Spec.Size = resource.MustParse("1Gi")
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = reconcileVolume(t, r, got)
	if got.Status.Phase != shpyrdv1.VolumeFailed || !strings.Contains(got.Status.Message, "cannot shrink") {
		t.Errorf("shrink: %+v", got.Status)
	}

	// Mode change: refused.
	got.Spec.Size = resource.MustParse("10Gi")
	got.Spec.AccessMode = corev1.ReadWriteMany
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = reconcileVolume(t, r, got)
	if got.Status.Phase != shpyrdv1.VolumeFailed || !strings.Contains(got.Status.Message, "access mode cannot change") {
		t.Errorf("mode change: %+v", got.Status)
	}

	// Class without expansion.
	fixed := testVolume("fixed", "1Gi", "")
	fixed.Spec.StorageClass = "local"
	r2, c2 := newVolumeReconciler(t, fixed, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local"}, Provisioner: "x"})
	fixed = reconcileVolume(t, r2, fixed)
	fixed.Spec.Size = resource.MustParse("2Gi")
	if err := c2.Update(context.Background(), fixed); err != nil {
		t.Fatal(err)
	}
	fixed = reconcileVolume(t, r2, fixed)
	if fixed.Status.Phase != shpyrdv1.VolumeFailed || !strings.Contains(fixed.Status.Message, `"local" does not allow volume expansion`) {
		t.Errorf("no expansion: %+v", fixed.Status)
	}
}

func TestAppMountsSingleInstanceVolume(t *testing.T) {
	vol := testVolume("data", "5Gi", "")
	shared := testVolume("assets", "5Gi", corev1.ReadWriteMany)
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "app-demo", Generation: 1},
		Spec: shpyrdv1.AppSpec{
			Image: "ghcr.io/o/demo:1",
			Processes: map[string]shpyrdv1.Process{
				"web":    {Replicas: ptr.To[int32](1), Volumes: []shpyrdv1.VolumeMount{{Name: "data", Path: "/data"}, {Name: "assets", Path: "/srv/assets"}}},
				"worker": {Replicas: ptr.To[int32](3), Command: []string{"worker"}, Volumes: []shpyrdv1.VolumeMount{{Name: "assets", Path: "/srv/assets/"}}},
			},
		},
	}
	r, c := newTestReconciler(t, app, vol, shared)
	got := runReconcile(t, r, app)
	if got.Status.Phase == shpyrdv1.PhaseFailed {
		t.Fatalf("unexpected failure: %s", got.Status.Message)
	}
	web := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "demo-web"}, web); err != nil {
		t.Fatal(err)
	}
	if web.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || *web.Spec.Replicas != 1 {
		t.Errorf("web must be Recreate with 1 replica: %s/%d", web.Spec.Strategy.Type, *web.Spec.Replicas)
	}
	if len(web.Spec.Template.Spec.Volumes) != 2 || web.Spec.Template.Spec.Volumes[1].PersistentVolumeClaim.ClaimName != "vol-data" {
		t.Errorf("web volumes = %+v", web.Spec.Template.Spec.Volumes)
	}
	if vm := web.Spec.Template.Spec.Containers[0].VolumeMounts; len(vm) != 2 || vm[1].MountPath != "/data" || vm[0].MountPath != "/srv/assets" {
		t.Errorf("web mounts = %+v", vm)
	}
	if got.Status.Processes["web"].Pinned != "single-instance volume data" {
		t.Errorf("pinned note = %q", got.Status.Processes["web"].Pinned)
	}
	worker := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "demo-worker"}, worker); err != nil {
		t.Fatal(err)
	}
	if worker.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || *worker.Spec.Replicas != 3 || got.Status.Processes["worker"].Pinned != "" {
		t.Errorf("shared volumes keep rolling updates and replicas: %s/%d", worker.Spec.Strategy.Type, *worker.Spec.Replicas)
	}

	// Scaling a single-instance process is refused with an explanation.
	got.Spec.Processes["web"] = shpyrdv1.Process{Replicas: ptr.To[int32](2), Volumes: []shpyrdv1.VolumeMount{{Name: "data", Path: "/data"}}}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, got)
	if got.Status.Phase != shpyrdv1.PhaseFailed || !strings.Contains(got.Status.Message, "can run 1 instance, not 2") {
		t.Errorf("scale refusal: %s %s", got.Status.Phase, got.Status.Message)
	}

	// Two processes on one RWO volume: refused.
	got.Spec.Processes["web"] = shpyrdv1.Process{Volumes: []shpyrdv1.VolumeMount{{Name: "data", Path: "/data"}}}
	got.Spec.Processes["worker"] = shpyrdv1.Process{Command: []string{"worker"}, Volumes: []shpyrdv1.VolumeMount{{Name: "data", Path: "/data"}}}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, got)
	if got.Status.Phase != shpyrdv1.PhaseFailed || !strings.Contains(got.Status.Message, "mounted by both web and worker") {
		t.Errorf("conflict refusal: %s", got.Status.Message)
	}

	// Unknown volume: helpful error.
	got.Spec.Processes = map[string]shpyrdv1.Process{"web": {Volumes: []shpyrdv1.VolumeMount{{Name: "missing", Path: "/x"}}}}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = runReconcile(t, r, got)
	if !strings.Contains(got.Status.Message, "shpyrd volumes create missing") {
		t.Errorf("missing volume: %s", got.Status.Message)
	}
	if reqs := r.volumeToApps(context.Background(), vol); len(reqs) != 0 {
		t.Errorf("volumeToApps should only list mounting apps, got %+v", reqs)
	}
}

package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

func TestPostgresReconcile(t *testing.T) {
	storage := resource.MustParse("10Gi")
	pg := &shpyrdv1.Postgres{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app-shop", Generation: 1},
		Spec:       shpyrdv1.PostgresSpec{Version: "16", Size: "shared-m", Storage: &storage, Instances: ptr.To[int32](2)},
	}
	base, c := newTestReconciler(t, pg)
	r := &PostgresReconciler{Client: c, Scheme: base.Scheme, Recorder: record.NewFakeRecorder(20), SystemNamespace: "shpyrd-system"}
	key := types.NamespacedName{Namespace: "app-shop", Name: "db"}
	run := func() *shpyrdv1.Postgres {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		out := &shpyrdv1.Postgres{}
		_ = c.Get(context.Background(), key, out)
		return out
	}

	got := run()
	if got.Status.Phase != shpyrdv1.ResourceProvisioning || got.Status.Endpoint != "db-rw.app-shop.svc:5432" {
		t.Fatalf("status = %+v", got.Status)
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGClusterGVK)
	if err := c.Get(context.Background(), key, cluster); err != nil {
		t.Fatalf("cnpg cluster: %v", err)
	}
	inst, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	img, _, _ := unstructured.NestedString(cluster.Object, "spec", "imageName")
	size, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "size")
	owner, _, _ := unstructured.NestedString(cluster.Object, "spec", "bootstrap", "initdb", "owner")
	cpu, _, _ := unstructured.NestedString(cluster.Object, "spec", "resources", "requests", "cpu")
	if inst != 2 || img != "ghcr.io/cloudnative-pg/postgresql:16" || size != "10Gi" || owner != "app" || cpu == "" {
		t.Errorf("cluster spec = %v", cluster.Object["spec"])
	}
	// Small sizes are raised to the PostgreSQL minimum.
	small := &shpyrdv1.Postgres{ObjectMeta: metav1.ObjectMeta{Name: "tiny", Namespace: "app-shop"}, Spec: shpyrdv1.PostgresSpec{Size: "shared-xs"}}
	if err := c.Create(context.Background(), small); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app-shop", Name: "tiny"}}); err != nil {
		t.Fatal(err)
	}
	tiny := &unstructured.Unstructured{}
	tiny.SetGroupVersionKind(CNPGClusterGVK)
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "tiny"}, tiny)
	if mem, _, _ := unstructured.NestedString(tiny.Object, "spec", "resources", "limits", "memory"); mem != "256Mi" {
		t.Errorf("memory floor not applied: %q", mem)
	}
	if len(cluster.GetOwnerReferences()) != 1 || cluster.GetOwnerReferences()[0].Kind != "Postgres" {
		t.Error("cluster must be owned by the Postgres")
	}

	// The binder waits while provisioning.
	if _, err := (PostgresBinder{}).ConfigVars(context.Background(), c, "app-shop", "db", "DATABASE"); err == nil || !strings.Contains(err.Error(), "provisioning") {
		t.Errorf("binder while provisioning: %v", err)
	}

	// CNPG reports a ready instance and creates the app Secret.
	_ = unstructured.SetNestedField(cluster.Object, int64(1), "status", "readyInstances")
	_ = unstructured.SetNestedField(cluster.Object, "Cluster in healthy state", "status", "phase")
	if err := c.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db-app", Namespace: "app-shop"}, Data: map[string][]byte{
		"username": []byte("app"), "password": []byte("s3cret"), "dbname": []byte("app"), "host": []byte("db-rw"), "port": []byte("5432"),
		"uri": []byte("postgresql://app:s3cret@db-rw:5432/app"),
	}}
	if err := c.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	got = run()
	if got.Status.Phase != shpyrdv1.ResourceReady || !strings.Contains(got.Status.Message, "1/2") {
		t.Fatalf("status = %+v", got.Status)
	}
	vars, err := (PostgresBinder{}).ConfigVars(context.Background(), c, "app-shop", "db", "DATABASE")
	if err != nil {
		t.Fatal(err)
	}
	if vars["DATABASE_URL"] != "postgresql://app:s3cret@db-rw:5432/app" || vars["DATABASE_HOST"] != "db-rw" || vars["DATABASE_NAME"] != "app" || vars["DATABASE_PASSWORD"] != "s3cret" {
		t.Errorf("vars = %v", vars)
	}

	// Growth is applied, shrinking refused.
	bigger := resource.MustParse("20Gi")
	got.Spec.Storage = &bigger
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = run()
	_ = c.Get(context.Background(), key, cluster)
	if size, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "size"); size != "20Gi" {
		t.Errorf("storage not grown: %s", size)
	}
	smaller := resource.MustParse("1Gi")
	got.Spec.Storage = &smaller
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = run()
	if got.Status.Phase != shpyrdv1.ResourceFailed || !strings.Contains(got.Status.Message, "cannot shrink") {
		t.Errorf("shrink: %+v", got.Status)
	}
}

func TestRedisReconcile(t *testing.T) {
	rd := &shpyrdv1.Redis{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "app-shop", Generation: 1},
		Spec:       shpyrdv1.RedisSpec{Size: "shared-s"},
	}
	storage := resource.MustParse("2Gi")
	queue := &shpyrdv1.Redis{
		ObjectMeta: metav1.ObjectMeta{Name: "queue", Namespace: "app-shop", Generation: 1},
		Spec:       shpyrdv1.RedisSpec{Engine: "redis", Persistent: true, Storage: &storage},
	}
	base, c := newTestReconciler(t, rd, queue)
	r := &RedisReconciler{Client: c, Scheme: base.Scheme, Recorder: record.NewFakeRecorder(20), SystemNamespace: "shpyrd-system"}
	run := func(name string) *shpyrdv1.Redis {
		key := types.NamespacedName{Namespace: "app-shop", Name: name}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
		out := &shpyrdv1.Redis{}
		_ = c.Get(context.Background(), key, out)
		return out
	}

	got := run("cache")
	if got.Status.Phase != shpyrdv1.ResourceProvisioning || got.Status.Endpoint != "cache.app-shop.svc:6379" {
		t.Fatalf("status = %+v", got.Status)
	}
	sts := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "cache"}, sts); err != nil {
		t.Fatal(err)
	}
	ct := sts.Spec.Template.Spec.Containers[0]
	args := strings.Join(ct.Args, " ")
	if ct.Image != "docker.io/valkey/valkey:8.1.10-alpine" || !strings.HasPrefix(args, "valkey-server --requirepass $(REDIS_PASSWORD)") || !strings.Contains(args, "allkeys-lru") || !strings.Contains(args, "--maxmemory ") || strings.Contains(args, "appendonly") {
		t.Errorf("cache container = %s %s", ct.Image, args)
	}
	if *ct.SecurityContext.RunAsUser != 999 || len(sts.Spec.VolumeClaimTemplates) != 0 {
		t.Errorf("cache pod: uid=%d claims=%d", *ct.SecurityContext.RunAsUser, len(sts.Spec.VolumeClaimTemplates))
	}
	sec := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "cache-redis"}, sec); err != nil || len(sec.Data["password"]) == 0 || !strings.HasPrefix(string(sec.Data["url"]), "redis://:") {
		t.Errorf("credentials: %v %v", err, sec.Data)
	}
	pw := string(sec.Data["password"])

	// The persistent redis queue gets a volume, AOF and no eviction.
	q := run("queue")
	qsts := &appsv1.StatefulSet{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "queue"}, qsts)
	qargs := strings.Join(qsts.Spec.Template.Spec.Containers[0].Args, " ")
	if qsts.Spec.Template.Spec.Containers[0].Image != "docker.io/library/redis:7.4-alpine" || !strings.HasPrefix(qargs, "redis-server") || !strings.Contains(qargs, "--appendonly yes") || !strings.Contains(qargs, "noeviction") {
		t.Errorf("queue container = %s", qargs)
	}
	if len(qsts.Spec.VolumeClaimTemplates) != 1 || qsts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String() != "2Gi" {
		t.Errorf("queue claims = %+v", qsts.Spec.VolumeClaimTemplates)
	}
	if !strings.Contains(q.Status.Message, "persistent (2Gi)") {
		t.Errorf("queue status = %+v", q.Status)
	}

	// Ready once the StatefulSet reports it; the binder then hands out the URL.
	sts.Status.ReadyReplicas = 1
	sts.Status.ObservedGeneration = sts.Generation
	if err := c.Status().Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
	got = run("cache")
	if got.Status.Phase != shpyrdv1.ResourceReady || !strings.Contains(got.Status.Message, "cache (data is lost on restart)") {
		t.Fatalf("status = %+v", got.Status)
	}
	vars, err := (RedisBinder{}).ConfigVars(context.Background(), c, "app-shop", "cache", "REDIS")
	if err != nil {
		t.Fatal(err)
	}
	if vars["REDIS_URL"] != "redis://:"+pw+"@cache:6379/0" || vars["REDIS_HOST"] != "cache" || vars["REDIS_PORT"] != "6379" || vars["REDIS_PASSWORD"] != pw {
		t.Errorf("vars = %v", vars)
	}
	// The password is stable across reconciles.
	run("cache")
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "cache-redis"}, sec)
	if string(sec.Data["password"]) != pw {
		t.Error("password must not change on reconcile")
	}
	// Changing persistence after creation is refused.
	got = run("cache")
	got.Spec.Persistent = true
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = run("cache")
	if got.Status.Phase != shpyrdv1.ResourceFailed || !strings.Contains(got.Status.Message, "persistence cannot change") {
		t.Errorf("persistence change: %+v", got.Status)
	}
	if _, _, _, err := redisEngine(&shpyrdv1.Redis{Spec: shpyrdv1.RedisSpec{Engine: "memcached"}}); err == nil {
		t.Error("unknown engine accepted")
	}
	var _ client.Object = got
}

// RFC-0060: datastores claim on the profile's class and are rounded up to
// the provider minimum, which the status reports.
func TestDatastoresFollowStorageProfile(t *testing.T) {
	pgStorage := resource.MustParse("5Gi")
	pg := &shpyrdv1.Postgres{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app-shop", Generation: 1},
		Spec:       shpyrdv1.PostgresSpec{Storage: &pgStorage},
	}
	rdStorage := resource.MustParse("1Gi")
	rd := &shpyrdv1.Redis{
		ObjectMeta: metav1.ObjectMeta{Name: "queue", Namespace: "app-shop", Generation: 1},
		Spec:       shpyrdv1.RedisSpec{Persistent: true, Storage: &rdStorage},
	}
	base, c := newTestReconciler(t, pg, rd)
	profile := StorageProfile{Class: "oci-bv", MinSize: "50Gi"}
	pgr := &PostgresReconciler{Client: c, Scheme: base.Scheme, Recorder: record.NewFakeRecorder(20), SystemNamespace: "shpyrd-system", Storage: profile}
	rdr := &RedisReconciler{Client: c, Scheme: base.Scheme, Recorder: record.NewFakeRecorder(20), SystemNamespace: "shpyrd-system", Storage: profile}

	key := types.NamespacedName{Namespace: "app-shop", Name: "db"}
	if _, err := pgr.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGClusterGVK)
	if err := c.Get(context.Background(), key, cluster); err != nil {
		t.Fatal(err)
	}
	size, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "size")
	class, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "storageClass")
	if size != "50Gi" || class != "oci-bv" {
		t.Errorf("cnpg storage = %v", cluster.Object["spec"].(map[string]interface{})["storage"])
	}
	_ = c.Get(context.Background(), key, pg)
	if pg.Status.Storage != "50Gi" || pg.Spec.Storage.String() != "5Gi" {
		t.Errorf("postgres status.storage = %q spec = %s", pg.Status.Storage, pg.Spec.Storage.String())
	}

	key = types.NamespacedName{Namespace: "app-shop", Name: "queue"}
	if _, err := rdr.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	sts := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), key, sts); err != nil {
		t.Fatal(err)
	}
	claim := sts.Spec.VolumeClaimTemplates[0].Spec
	if claim.Resources.Requests.Storage().String() != "50Gi" || claim.StorageClassName == nil || *claim.StorageClassName != "oci-bv" {
		t.Errorf("redis claim = %+v", claim)
	}
	_ = c.Get(context.Background(), key, rd)
	if rd.Status.Storage != "50Gi" {
		t.Errorf("redis status.storage = %q", rd.Status.Storage)
	}

	// Without a profile nothing changes.
	plain := &PostgresReconciler{Client: c, Scheme: base.Scheme, Recorder: record.NewFakeRecorder(20), SystemNamespace: "shpyrd-system"}
	if got, applied := plain.Storage.Size(pgStorage); applied || got.String() != "5Gi" {
		t.Errorf("no profile: %s %v", got.String(), applied)
	}
}

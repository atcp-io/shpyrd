package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/kube"
	"shpyrd/pkg/objectstore"
)

// fakeStore records what the controller asked of the object store.
type fakeStore struct {
	buckets map[string]objectstore.BucketSpec
	users   map[string]string // access key -> bucket
	secrets map[string]string // access key -> secret key
	deleted []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{buckets: map[string]objectstore.BucketSpec{}, users: map[string]string{}, secrets: map[string]string{}}
}
func (f *fakeStore) EnsureLayout(context.Context, int64) error { return nil }
func (f *fakeStore) EnsureBucket(_ context.Context, spec objectstore.BucketSpec) error {
	f.buckets[spec.Name] = spec
	return nil
}
func (f *fakeStore) DeleteBucket(_ context.Context, name string) error {
	delete(f.buckets, name)
	f.deleted = append(f.deleted, name)
	return nil
}
func (f *fakeStore) EnsureUser(_ context.Context, bucket, ak, sk string) (objectstore.Credential, error) {
	if ak == "" {
		ak = "AK" + bucket
	}
	if sk == "" {
		sk = "SK" + bucket
	}
	f.users[ak] = bucket
	f.secrets[ak] = sk
	return objectstore.Credential{AccessKey: ak, SecretKey: sk}, nil
}
func (f *fakeStore) DeleteUser(_ context.Context, _ string, ak string) error {
	delete(f.users, ak)
	return nil
}
func (f *fakeStore) Usage(context.Context) (*objectstore.Capacity, error) {
	out := &objectstore.Capacity{TotalBytes: 100, UsedBytes: 10, Buckets: map[string]objectstore.Usage{}}
	for name := range f.buckets {
		out.Buckets[name] = objectstore.Usage{Bytes: 42, Objects: 3}
	}
	return out, nil
}

// RFC-0046: an ObjectBucket gets its bucket, a scoped user and a Secret in
// its namespace; the credential stays stable; deletion cleans up.
func TestObjectBucketLifecycle(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	root := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "object-storage-admin", Namespace: "shpyrd-system"}, Data: map[string][]byte{"adminToken": []byte("tok")}}
	bucket := &shpyrdv1.ObjectBucket{ObjectMeta: metav1.ObjectMeta{Name: "backups", Namespace: "app-shop"}, Spec: shpyrdv1.ObjectBucketSpec{RetentionDays: 30, Versioning: true}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, bucket).WithStatusSubresource(&shpyrdv1.ObjectBucket{}).Build()
	store := newFakeStore()
	r := &ObjectBucketReconciler{
		Client: c, Reader: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		SystemNamespace: "shpyrd-system", Endpoint: "http://object-storage.shpyrd-system.svc:3900", AdminEndpoint: "http://object-storage.shpyrd-system.svc:3903", AdminSecret: "object-storage-admin",
		Connect: func(endpoint, admin, token string) (BucketStore, error) {
			if token != "tok" || endpoint == "" || admin == "" {
				t.Errorf("admin credential not used: %s %s %s", endpoint, admin, token)
			}
			return store, nil
		},
	}
	key := types.NamespacedName{Namespace: "app-shop", Name: "backups"}
	ctx := context.Background()
	run := func() *shpyrdv1.ObjectBucket {
		for i := 0; i < 3; i++ { // finalizer, then the work
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
		}
		out := &shpyrdv1.ObjectBucket{}
		_ = c.Get(ctx, key, out)
		return out
	}

	got := run()
	if got.Status.Phase != shpyrdv1.BucketReady || got.Status.Bucket != "shpyrd-app-shop-backups" || got.Status.SecretName != "backups-object-storage" || got.Status.UsedBytes != 42 {
		t.Fatalf("status = %+v", got.Status)
	}
	spec := store.buckets["shpyrd-app-shop-backups"]
	if !spec.Versioning || spec.RetentionDays != 30 {
		t.Errorf("bucket spec = %+v", spec)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "app-shop", Name: "backups-object-storage"}, sec); err != nil {
		t.Fatal(err)
	}
	if sec.StringData["BUCKET"] != "shpyrd-app-shop-backups" || sec.StringData["AWS_ENDPOINT_URL"] != r.Endpoint || sec.StringData["accessKeyId"] != sec.StringData["AWS_ACCESS_KEY_ID"] || len(sec.OwnerReferences) != 1 {
		t.Errorf("secret = %v owners=%d", sec.StringData, len(sec.OwnerReferences))
	}
	if store.users[sec.StringData["AWS_ACCESS_KEY_ID"]] != "shpyrd-app-shop-backups" {
		t.Errorf("user not scoped to the bucket: %v", store.users)
	}

	// A second pass keeps the same credential (the fake client keeps
	// StringData, which the controller reads back as Data in a real cluster).
	first := sec.StringData["AWS_ACCESS_KEY_ID"]
	run()
	if len(store.users) != 1 {
		t.Errorf("credential must stay stable, users = %v", store.users)
	}
	_ = first

	// Deletion: user and bucket go, then the finalizer.
	if err := c.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	run()
	if err := c.Get(ctx, key, &shpyrdv1.ObjectBucket{}); !apierrors.IsNotFound(err) {
		t.Errorf("bucket resource should be gone: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "shpyrd-app-shop-backups" || len(store.users) != 0 {
		t.Errorf("cleanup: deleted=%v users=%v", store.deleted, store.users)
	}

	// Retain keeps the bucket's contents.
	keep := &shpyrdv1.ObjectBucket{ObjectMeta: metav1.ObjectMeta{Name: "keep", Namespace: "app-shop"}, Spec: shpyrdv1.ObjectBucketSpec{DeletionPolicy: "Retain"}}
	if err := c.Create(ctx, keep); err != nil {
		t.Fatal(err)
	}
	key = types.NamespacedName{Namespace: "app-shop", Name: "keep"}
	run()
	if err := c.Delete(ctx, keep); err != nil {
		t.Fatal(err)
	}
	run()
	if _, still := store.buckets["shpyrd-app-shop-keep"]; !still {
		t.Error("Retain must leave the bucket")
	}
	var _ client.Client = c
}

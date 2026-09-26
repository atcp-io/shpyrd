package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/objectstore"
)

// ObjectBucketFinalizer guards the store-side cleanup.
const ObjectBucketFinalizer = "shpyrd.io/object-bucket"

// StoreConnector opens the object store as administrator; a variable so
// tests can substitute a fake.
type StoreConnector func(endpoint, adminEndpoint, adminToken string) (BucketStore, error)

// BucketStore is what the controller needs from the store.
type BucketStore interface {
	EnsureLayout(ctx context.Context, capacityBytes int64) error
	EnsureBucket(ctx context.Context, spec objectstore.BucketSpec) error
	DeleteBucket(ctx context.Context, name string) error
	EnsureUser(ctx context.Context, bucket, accessKey, secretKey string) (objectstore.Credential, error)
	DeleteUser(ctx context.Context, bucket, accessKey string) error
	Usage(ctx context.Context) (*objectstore.Capacity, error)
}

// ObjectBucketReconciler gives every ObjectBucket its bucket and scoped
// credential in the platform's object store (RFC-0046).
type ObjectBucketReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Reader bypasses the cache for the admin Secret.
	Reader client.Reader
	// SystemNamespace holds the admin Secret (key adminToken); Endpoint is
	// the S3 URL consumers use, AdminEndpoint the admin API.
	SystemNamespace string
	Endpoint        string
	AdminEndpoint   string
	AdminSecret     string
	// CapacityBytes of the store's volume, for the layout of a fresh store.
	CapacityBytes int64
	Connect       StoreConnector

	mu    sync.Mutex
	store BucketStore
}

func (r *ObjectBucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&shpyrdv1.ObjectBucket{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// Store returns the connected store, connecting on first use.
func (r *ObjectBucketReconciler) Store(ctx context.Context) (BucketStore, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.store != nil {
		return r.store, nil
	}
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	sec := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: r.SystemNamespace, Name: r.AdminSecret}, sec); err != nil {
		return nil, fmt.Errorf("object storage is not installed yet (no admin credential): %w", err)
	}
	connect := r.Connect
	if connect == nil {
		connect = func(endpoint, admin, token string) (BucketStore, error) {
			return objectstore.Connect(endpoint, admin, token)
		}
	}
	s, err := connect(r.Endpoint, r.AdminEndpoint, string(sec.Data["adminToken"]))
	if err != nil {
		return nil, err
	}
	// A fresh store holds nothing until its node has a role.
	if err := s.EnsureLayout(ctx, r.CapacityBytes); err != nil {
		return nil, fmt.Errorf("object storage layout: %w", err)
	}
	r.store = s
	return s, nil
}

func (r *ObjectBucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	b := &shpyrdv1.ObjectBucket{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	store, err := r.Store(ctx)
	if err != nil {
		if !b.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(b, ObjectBucketFinalizer) {
			return ctrl.Result{}, nil
		}
		return r.fail(ctx, b, err)
	}

	if !b.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(b, ObjectBucketFinalizer) {
			if err := r.cleanup(ctx, store, b); err != nil {
				logger.Error(err, "object bucket cleanup")
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			controllerutil.RemoveFinalizer(b, ObjectBucketFinalizer)
			if err := r.Update(ctx, b); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(b, ObjectBucketFinalizer) {
		controllerutil.AddFinalizer(b, ObjectBucketFinalizer)
		if err := r.Update(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
	}

	orig := b.DeepCopy()
	if err := r.reconcile(ctx, store, b); err != nil {
		return r.fail(ctx, b, err)
	}
	b.Status.ObservedGeneration = b.Generation
	if err := r.Status().Patch(ctx, b, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *ObjectBucketReconciler) reconcile(ctx context.Context, store BucketStore, b *shpyrdv1.ObjectBucket) error {
	bucket := b.BucketName()
	if err := store.EnsureBucket(ctx, objectstore.BucketSpec{Name: bucket, Versioning: b.Spec.Versioning, RetentionDays: b.Spec.RetentionDays}); err != nil {
		return err
	}
	// Keep the credential stable across reconciles: reuse what the Secret holds.
	secretName := b.CredentialSecretName()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: b.Namespace}}
	var accessKey, secretKey string
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: secretName}, sec); err == nil {
		accessKey, secretKey = credentialFrom(sec)
	}
	cred, err := store.EnsureUser(ctx, bucket, accessKey, secretKey)
	if err != nil {
		return err
	}
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: b.Namespace, Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", "shpyrd.io/object-bucket": b.Name}},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"AWS_ACCESS_KEY_ID":     cred.AccessKey,
			"AWS_SECRET_ACCESS_KEY": cred.SecretKey,
			"AWS_ENDPOINT_URL":      r.Endpoint,
			"AWS_REGION":            objectstore.Region,
			"BUCKET":                bucket,
			// Consumers that name their keys (CloudNativePG's ObjectStore).
			"accessKeyId":     cred.AccessKey,
			"secretAccessKey": cred.SecretKey,
		},
	}
	if err := controllerutil.SetControllerReference(b, desired, r.Scheme); err != nil {
		return err
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = desired.Labels
		sec.Type = corev1.SecretTypeOpaque
		sec.StringData = desired.StringData
		sec.OwnerReferences = desired.OwnerReferences
		return nil
	}); err != nil {
		return fmt.Errorf("credential secret: %w", err)
	}

	b.Status.Bucket = bucket
	b.Status.Endpoint = r.Endpoint
	b.Status.SecretName = secretName
	if b.Status.Phase != shpyrdv1.BucketReady {
		r.Recorder.Eventf(b, corev1.EventTypeNormal, "Ready", "bucket %s with credential %s", bucket, secretName)
	}
	b.Status.Phase = shpyrdv1.BucketReady
	b.Status.Message = ""
	if cap, err := store.Usage(ctx); err == nil {
		if u, ok := cap.Buckets[bucket]; ok {
			b.Status.UsedBytes, b.Status.Objects = u.Bytes, u.Objects
			now := metav1.Now()
			b.Status.MeasuredAt = &now
		}
	}
	return nil
}

func (r *ObjectBucketReconciler) cleanup(ctx context.Context, store BucketStore, b *shpyrdv1.ObjectBucket) error {
	bucket := b.BucketName()
	sec := &corev1.Secret{}
	accessKey := ""
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.CredentialSecretName()}, sec); err == nil {
		accessKey, _ = credentialFrom(sec)
	}
	if err := store.DeleteUser(ctx, bucket, accessKey); err != nil {
		return err
	}
	if b.Spec.DeletionPolicy != "Retain" {
		if err := store.DeleteBucket(ctx, bucket); err != nil {
			return err
		}
	}
	return nil
}

func (r *ObjectBucketReconciler) fail(ctx context.Context, b *shpyrdv1.ObjectBucket, err error) (ctrl.Result, error) {
	orig := b.DeepCopy()
	b.Status.Phase = shpyrdv1.BucketFailed
	b.Status.Message = err.Error()
	b.Status.ObservedGeneration = b.Generation
	if perr := r.Status().Patch(ctx, b, client.MergeFrom(orig)); perr != nil && !apierrors.IsNotFound(perr) {
		return ctrl.Result{}, perr
	}
	r.Recorder.Event(b, corev1.EventTypeWarning, "ReconcileError", err.Error())
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// credentialFrom reads the access key pair from a credential Secret (Data
// in a cluster; StringData right after a write, as fake clients keep it).
func credentialFrom(sec *corev1.Secret) (string, string) {
	get := func(k string) string {
		if v, ok := sec.Data[k]; ok && len(v) > 0 {
			return string(v)
		}
		return sec.StringData[k]
	}
	return get("AWS_ACCESS_KEY_ID"), get("AWS_SECRET_ACCESS_KEY")
}

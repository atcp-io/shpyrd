package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// PostgresReconciler turns a Postgres resource into a CloudNativePG Cluster
// (RFC-0009). CNPG runs PostgreSQL, creates the application database and
// user and keeps their credentials in Secret <name>-app; the resource
// status summarises the cluster and the Binder turns the Secret into
// config vars.
type PostgresReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	SystemNamespace string
	// Storage is the profile's disk rules (RFC-0060).
	Storage StorageProfile
}

// cnpgStorage is the CNPG storage section: the size and, when the profile
// names one, the class (RFC-0060).
func cnpgStorage(size resource.Quantity, class string) map[string]interface{} {
	out := map[string]interface{}{"size": size.String()}
	if class != "" {
		out["storageClass"] = class
	}
	return out
}

// CNPGClusterGVK is CloudNativePG's Cluster (handled as unstructured).
var CNPGClusterGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}

const (
	defaultPostgresVersion = "17"
	defaultPostgresStorage = "5Gi"
	// postgresMinMemory is the least PostgreSQL runs (and initdb completes)
	// with; small catalog sizes are raised to it.
	postgresMinMemory = "256Mi"
	// PostgresDatabase and PostgresUser are what CNPG's initdb bootstrap creates.
	PostgresDatabase = "app"
	PostgresUser     = "app"
	PostgresPort     = 5432
)

func (r *PostgresReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGClusterGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&shpyrdv1.Postgres{}).
		Owns(cluster).
		Complete(r)
}

func (r *PostgresReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	pg := &shpyrdv1.Postgres{}
	if err := r.Get(ctx, req.NamespacedName, pg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pg.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := pg.DeepCopy()
	res, err := r.reconcile(ctx, pg)
	if apierrors.IsConflict(err) {
		return ctrl.Result{Requeue: true}, nil
	}
	if err != nil {
		logger.Error(err, "reconcile postgres failed")
		pg.Status.Phase = shpyrdv1.ResourceFailed
		pg.Status.Message = err.Error()
		setResourceCondition(&pg.Status, pg.Generation, metav1.ConditionFalse, "Error", err.Error())
		r.Recorder.Event(pg, corev1.EventTypeWarning, "ReconcileError", err.Error())
		res = ctrl.Result{RequeueAfter: 30 * time.Second}
	}
	pg.Status.ObservedGeneration = pg.Generation
	if statusErr := r.Status().Patch(ctx, pg, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); statusErr != nil {
		if apierrors.IsConflict(statusErr) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, statusErr
	}
	return res, nil
}

func (r *PostgresReconciler) reconcile(ctx context.Context, pg *shpyrdv1.Postgres) (ctrl.Result, error) {
	catalog := loadCatalog(ctx, r.Client, r.SystemNamespace)
	resources, _, err := catalog.Resolve(pg.Spec.Size, corev1.ResourceRequirements{})
	if err != nil {
		return ctrl.Result{}, err
	}
	resources = withMemoryFloor(resources, resource.MustParse(postgresMinMemory))
	storage := resource.MustParse(defaultPostgresStorage)
	if pg.Spec.Storage != nil {
		storage = *pg.Spec.Storage
	}
	if storage.Sign() <= 0 {
		return ctrl.Result{}, fmt.Errorf("storage must be positive")
	}
	// The provider minimum (RFC-0060): the disk is that size whatever was
	// asked; the status says so.
	pg.Status.Storage = ""
	if rounded, applied := r.Storage.Size(storage); applied {
		storage = rounded
		pg.Status.Storage = rounded.String()
	}

	desired := desiredCNPGCluster(pg, storage, resources, r.Storage.Class)
	if err := controllerutil.SetControllerReference(pg, desired, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(CNPGClusterGVK)
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), current)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil {
			if strings.Contains(err.Error(), "no matches for kind") {
				return ctrl.Result{}, fmt.Errorf("the postgres extension is not installed on this cluster (CloudNativePG missing): run `shpyrd extensions enable postgres`")
			}
			return ctrl.Result{}, fmt.Errorf("create database cluster: %w", err)
		}
		r.Recorder.Eventf(pg, corev1.EventTypeNormal, "Provisioning", "created PostgreSQL %s cluster %s (%s, %d instance(s))", version(pg), desired.GetName(), storage.String(), instances(pg))
		current = desired
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get database cluster: %w", err)
	default:
		// Apply the fields we own; CNPG owns the rest of the spec.
		if err := r.updateCluster(ctx, pg, current, desired); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Status from the CNPG cluster and its application Secret.
	pg.Status.Endpoint = fmt.Sprintf("%s-rw.%s.svc:%d", pg.Name, pg.Namespace, PostgresPort)
	pg.Status.CredentialsSecret = PostgresSecretName(pg.Name)
	ready, _, _ := unstructured.NestedInt64(current.Object, "status", "readyInstances")
	phase, _, _ := unstructured.NestedString(current.Object, "status", "phase")
	reason, _, _ := unstructured.NestedString(current.Object, "status", "phaseReason")
	secretExists := false
	if err := r.Get(ctx, types.NamespacedName{Namespace: pg.Namespace, Name: PostgresSecretName(pg.Name)}, &corev1.Secret{}); err == nil {
		secretExists = true
	}
	switch {
	case strings.Contains(strings.ToLower(phase), "failed") || strings.Contains(strings.ToLower(phase), "unrecoverable"):
		pg.Status.Phase = shpyrdv1.ResourceFailed
		pg.Status.Message = firstNonEmpty(reason, phase)
		setResourceCondition(&pg.Status, pg.Generation, metav1.ConditionFalse, "ClusterFailed", pg.Status.Message)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	case ready >= 1 && secretExists:
		pg.Status.Phase = shpyrdv1.ResourceReady
		pg.Status.Message = fmt.Sprintf("PostgreSQL %s, %d/%d instance(s) ready", version(pg), ready, instances(pg))
		setResourceCondition(&pg.Status, pg.Generation, metav1.ConditionTrue, "Ready", pg.Status.Message)
		if ready < int64(instances(pg)) {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	default:
		pg.Status.Phase = shpyrdv1.ResourceProvisioning
		pg.Status.Message = firstNonEmpty(phase, "creating the PostgreSQL cluster")
		setResourceCondition(&pg.Status, pg.Generation, metav1.ConditionFalse, "Provisioning", pg.Status.Message)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

// updateCluster applies instances, storage growth and resources.
func (r *PostgresReconciler) updateCluster(ctx context.Context, pg *shpyrdv1.Postgres, current, desired *unstructured.Unstructured) error {
	changed := false
	patch := client.MergeFrom(current.DeepCopy())
	if cur, _, _ := unstructured.NestedInt64(current.Object, "spec", "instances"); cur != int64(instances(pg)) {
		_ = unstructured.SetNestedField(current.Object, int64(instances(pg)), "spec", "instances")
		changed = true
	}
	wantSize, _, _ := unstructured.NestedString(desired.Object, "spec", "storage", "size")
	if curSize, _, _ := unstructured.NestedString(current.Object, "spec", "storage", "size"); curSize != wantSize {
		want := resource.MustParse(wantSize)
		if cur, err := resource.ParseQuantity(curSize); err == nil && want.Cmp(cur) < 0 {
			return fmt.Errorf("storage cannot shrink (currently %s)", curSize)
		}
		_ = unstructured.SetNestedField(current.Object, wantSize, "spec", "storage", "size")
		changed = true
	}
	wantRes, _, _ := unstructured.NestedMap(desired.Object, "spec", "resources")
	if curRes, _, _ := unstructured.NestedMap(current.Object, "spec", "resources"); !equalJSON(curRes, wantRes) {
		_ = unstructured.SetNestedMap(current.Object, wantRes, "spec", "resources")
		changed = true
	}
	if !changed {
		return nil
	}
	if err := r.Patch(ctx, current, patch); err != nil {
		return fmt.Errorf("update database cluster: %w", err)
	}
	r.Recorder.Event(pg, corev1.EventTypeNormal, "Updated", "database cluster settings applied")
	return nil
}

// withMemoryFloor raises memory requests and limits to at least floor.
func withMemoryFloor(res corev1.ResourceRequirements, floor resource.Quantity) corev1.ResourceRequirements {
	if res.Requests == nil {
		res.Requests = corev1.ResourceList{}
	}
	if res.Limits == nil {
		res.Limits = corev1.ResourceList{}
	}
	if q, ok := res.Requests[corev1.ResourceMemory]; !ok || q.Cmp(floor) < 0 {
		res.Requests[corev1.ResourceMemory] = floor
	}
	if q, ok := res.Limits[corev1.ResourceMemory]; !ok || q.Cmp(floor) < 0 {
		res.Limits[corev1.ResourceMemory] = floor
	}
	return res
}

// PostgresSecretName is the Secret CNPG creates for the application user.
func PostgresSecretName(name string) string { return name + "-app" }

func version(pg *shpyrdv1.Postgres) string {
	return firstNonEmpty(pg.Spec.Version, defaultPostgresVersion)
}

func instances(pg *shpyrdv1.Postgres) int32 {
	if pg.Spec.Instances != nil && *pg.Spec.Instances > 0 {
		return *pg.Spec.Instances
	}
	return 1
}

// desiredCNPGCluster renders the CloudNativePG Cluster for a Postgres.
func desiredCNPGCluster(pg *shpyrdv1.Postgres, storage resource.Quantity, res corev1.ResourceRequirements, storageClass string) *unstructured.Unstructured {
	toMap := func(l corev1.ResourceList) map[string]interface{} {
		out := map[string]interface{}{}
		for k, v := range l {
			out[string(k)] = v.String()
		}
		return out
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(CNPGClusterGVK)
	u.SetName(pg.Name)
	u.SetNamespace(pg.Namespace)
	u.SetLabels(map[string]string{
		shpyrdv1.LabelManagedBy: "shpyrd",
		"shpyrd.io/postgres":    pg.Name,
	})
	u.Object["spec"] = map[string]interface{}{
		"instances":             int64(instances(pg)),
		"imageName":             "ghcr.io/cloudnative-pg/postgresql:" + version(pg),
		"storage":               cnpgStorage(storage, storageClass),
		"resources":             map[string]interface{}{"requests": toMap(res.Requests), "limits": toMap(res.Limits)},
		"bootstrap":             map[string]interface{}{"initdb": map[string]interface{}{"database": PostgresDatabase, "owner": PostgresUser}},
		"enableSuperuserAccess": false,
	}
	return u
}

// setResourceCondition maintains the Ready condition of a resource status.
func setResourceCondition(st *shpyrdv1.ResourceStatus, generation int64, status metav1.ConditionStatus, reason, msg string) {
	cond := metav1.Condition{Type: shpyrdv1.ConditionReady, Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.Now(), ObservedGeneration: generation}
	for i, c := range st.Conditions {
		if c.Type == cond.Type {
			if c.Status == cond.Status {
				cond.LastTransitionTime = c.LastTransitionTime
			}
			st.Conditions[i] = cond
			return
		}
	}
	st.Conditions = append(st.Conditions, cond)
}

// ---- binding ----------------------------------------------------------------

// PostgresBinder exposes a Postgres as config vars (RFC-0003).
type PostgresBinder struct{}

// DefaultPrefix gives DATABASE_URL and friends.
func (PostgresBinder) DefaultPrefix() string { return "DATABASE" }

// ConfigVars reads CNPG's application Secret.
func (PostgresBinder) ConfigVars(ctx context.Context, c client.Client, namespace, name, prefix string) (map[string]string, error) {
	pg := &shpyrdv1.Postgres{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pg); err != nil {
		return nil, err
	}
	if pg.Status.Phase != shpyrdv1.ResourceReady {
		return nil, &NotReadyError{Msg: fmt.Sprintf("Postgres %s is %s", name, strings.ToLower(firstNonEmpty(pg.Status.Phase, "provisioning")))}
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: PostgresSecretName(name)}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &NotReadyError{Msg: fmt.Sprintf("Postgres %s has no credentials yet", name)}
		}
		return nil, err
	}
	get := func(k string) string { return string(sec.Data[k]) }
	host := firstNonEmpty(get("host"), name+"-rw")
	port := firstNonEmpty(get("port"), fmt.Sprint(PostgresPort))
	db := firstNonEmpty(get("dbname"), PostgresDatabase)
	user := firstNonEmpty(get("username"), PostgresUser)
	pass := get("password")
	url := firstNonEmpty(get("uri"), fmt.Sprintf("postgresql://%s:%s@%s:%s/%s", user, pass, host, port, db))
	return map[string]string{
		prefix + "_URL":      url,
		prefix + "_HOST":     host,
		prefix + "_PORT":     port,
		prefix + "_USER":     user,
		prefix + "_PASSWORD": pass,
		prefix + "_NAME":     db,
	}, nil
}

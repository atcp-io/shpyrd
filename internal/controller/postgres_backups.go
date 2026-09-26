package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
)

// Postgres backups (RFC-0038): continuous WAL archiving and scheduled base
// backups through CloudNativePG's Barman Cloud plugin, into a bucket of the
// platform's object store (RFC-0046) that only this database's namespace
// can open. Recovery bootstraps a new database from another one's backups.

// BarmanObjectStoreGVK is the plugin's store description.
var BarmanObjectStoreGVK = schema.GroupVersionKind{Group: "barmancloud.cnpg.io", Version: "v1", Kind: "ObjectStore"}

// CNPGScheduledBackupGVK and CNPGBackupGVK are CloudNativePG's backup kinds.
var (
	CNPGScheduledBackupGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "ScheduledBackup"}
	CNPGBackupGVK          = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Backup"}
)

// BarmanPluginName is the plugin's registered name.
const BarmanPluginName = "barman-cloud.cloudnative-pg.io"

const (
	defaultBackupSchedule  = "0 2 * * *"
	defaultBackupRetention = "14d"
)

// BackupsBucketName is the ObjectBucket (and its Secret prefix) of a
// database's backups.
func BackupsBucketName(pg string) string { return pg + "-backups" }

// backupSchedule returns the CNPG six-field schedule (with seconds) for the
// five-field cron users write.
func backupSchedule(spec *shpyrdv1.PostgresBackups) (string, error) {
	s := defaultBackupSchedule
	if spec != nil && spec.Schedule != "" {
		s = spec.Schedule
	}
	fields := strings.Fields(s)
	switch len(fields) {
	case 5:
		return "0 " + s, nil
	case 6:
		return s, nil
	}
	return "", fmt.Errorf("backups.schedule %q: five cron fields expected (minute hour day month weekday)", s)
}

func backupRetention(spec *shpyrdv1.PostgresBackups) (string, int32, error) {
	ret := defaultBackupRetention
	if spec != nil && spec.Retention != "" {
		ret = spec.Retention
	}
	n, err := strconv.Atoi(strings.TrimSuffix(ret, "d"))
	if err != nil || !strings.HasSuffix(ret, "d") || n <= 0 {
		return "", 0, fmt.Errorf("backups.retention %q: use <days>d, e.g. 14d", ret)
	}
	return ret, int32(n), nil
}

// reconcileBackups makes the bucket and the plugin's ObjectStore exist for a
// database with backups, plus its ScheduledBackup; without backups it
// removes the schedule (the bucket stays with its data until the database
// is deleted).
func (r *PostgresReconciler) reconcileBackups(ctx context.Context, pg *shpyrdv1.Postgres) error {
	bucket := &shpyrdv1.ObjectBucket{ObjectMeta: metav1.ObjectMeta{Name: BackupsBucketName(pg.Name), Namespace: pg.Namespace}}
	schedule := &unstructured.Unstructured{}
	schedule.SetGroupVersionKind(CNPGScheduledBackupGVK)
	schedule.SetName(pg.Name + "-scheduled")
	schedule.SetNamespace(pg.Namespace)

	if pg.Spec.Backups == nil {
		// Off: no schedule; the archive keeps what it has.
		if err := r.Delete(ctx, schedule); err != nil && !apierrors.IsNotFound(err) && !isNoKind(err) {
			return fmt.Errorf("remove backup schedule: %w", err)
		}
		return nil
	}
	cron, err := backupSchedule(pg.Spec.Backups)
	if err != nil {
		return err
	}
	retention, days, err := backupRetention(pg.Spec.Backups)
	if err != nil {
		return err
	}

	// 1. The bucket, with the platform's object store (RFC-0046). Retention
	// is the plugin's job (it knows which WALs a backup needs), so the
	// bucket keeps everything the plugin leaves.
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, bucket, func() error {
		bucket.Labels = map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", "shpyrd.io/postgres": pg.Name}
		bucket.Spec.DeletionPolicy = "Delete"
		return controllerutil.SetControllerReference(pg, bucket, r.Scheme)
	}); err != nil {
		if isNoKind(err) {
			return fmt.Errorf("backups need the object-storage extension: run `shpyrd extensions enable object-storage`")
		}
		return fmt.Errorf("backups bucket: %w", err)
	}
	if bucket.Status.Phase != shpyrdv1.BucketReady {
		return &pendingError{msg: "waiting for the backups bucket (" + firstNonEmpty(bucket.Status.Message, "provisioning") + ")"}
	}

	// 2. The plugin's ObjectStore pointing at it with the bucket's key.
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(BarmanObjectStoreGVK)
	store.SetName(BackupsBucketName(pg.Name))
	store.SetNamespace(pg.Namespace)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, store, func() error {
		store.SetLabels(map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", "shpyrd.io/postgres": pg.Name})
		secret := bucket.CredentialSecretName()
		store.Object["spec"] = map[string]interface{}{
			"retentionPolicy": retention,
			"configuration": map[string]interface{}{
				"destinationPath": "s3://" + bucket.Status.Bucket + "/",
				"endpointURL":     bucket.Status.Endpoint,
				"s3Credentials": map[string]interface{}{
					"accessKeyId":     map[string]interface{}{"name": secret, "key": "accessKeyId"},
					"secretAccessKey": map[string]interface{}{"name": secret, "key": "secretAccessKey"},
					"region":          map[string]interface{}{"name": secret, "key": "AWS_REGION"},
				},
				"wal":  map[string]interface{}{"compression": "gzip"},
				"data": map[string]interface{}{"compression": "gzip"},
			},
		}
		return controllerutil.SetControllerReference(pg, store, r.Scheme)
	}); err != nil {
		if isNoKind(err) {
			return fmt.Errorf("backups need the Barman Cloud plugin (component barman-cloud of the postgres extension): run `shpyrd cluster init`")
		}
		return fmt.Errorf("backups object store: %w", err)
	}

	// 3. The schedule. Base backups every day by default; the plugin
	// expires what falls outside the retention.
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, schedule, func() error {
		schedule.SetLabels(map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", "shpyrd.io/postgres": pg.Name})
		schedule.Object["spec"] = map[string]interface{}{
			"schedule":             cron,
			"immediate":            true,
			"backupOwnerReference": "self",
			"cluster":              map[string]interface{}{"name": pg.Name},
			"method":               "plugin",
			"pluginConfiguration":  map[string]interface{}{"name": BarmanPluginName},
		}
		return controllerutil.SetControllerReference(pg, schedule, r.Scheme)
	}); err != nil {
		return fmt.Errorf("backup schedule: %w", err)
	}
	_ = days
	return nil
}

// backupPlugin is the Cluster's plugins entry that archives WAL and takes
// backups into the database's ObjectStore.
func backupPlugin(pg *shpyrdv1.Postgres) map[string]interface{} {
	return map[string]interface{}{
		"name":          BarmanPluginName,
		"isWALArchiver": true,
		"enabled":       true,
		"parameters":    map[string]interface{}{"barmanObjectName": BackupsBucketName(pg.Name)},
	}
}

// recoveryBootstrap replaces initdb with a recovery from the source
// database's backups (the source's ObjectStore, in the same namespace).
func recoveryBootstrap(pg *shpyrdv1.Postgres) (bootstrap map[string]interface{}, external []interface{}) {
	rec := pg.Spec.Recovery
	recovery := map[string]interface{}{"source": rec.From, "database": PostgresDatabase, "owner": PostgresUser}
	if rec.TargetTime != nil {
		recovery["recoveryTarget"] = map[string]interface{}{"targetTime": rec.TargetTime.UTC().Format("2006-01-02 15:04:05+00")}
	}
	return map[string]interface{}{"recovery": recovery}, []interface{}{map[string]interface{}{
		"name": rec.From,
		"plugin": map[string]interface{}{
			"name":       BarmanPluginName,
			"enabled":    true,
			"parameters": map[string]interface{}{"barmanObjectName": BackupsBucketName(rec.From), "serverName": rec.From},
		},
	}}
}

// checkRecoverySource verifies the source database has backups to restore.
func (r *PostgresReconciler) checkRecoverySource(ctx context.Context, pg *shpyrdv1.Postgres) error {
	rec := pg.Spec.Recovery
	if rec.From == "" {
		return fmt.Errorf("recovery.from names the database to restore")
	}
	if rec.From == pg.Name {
		return fmt.Errorf("a database cannot be restored onto itself; restore to a new name")
	}
	source := &shpyrdv1.Postgres{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: pg.Namespace, Name: rec.From}, source); err != nil {
		if apierrors.IsNotFound(err) {
			// The source may be gone; its ObjectStore must still be here.
			store := &unstructured.Unstructured{}
			store.SetGroupVersionKind(BarmanObjectStoreGVK)
			if err := r.Get(ctx, types.NamespacedName{Namespace: pg.Namespace, Name: BackupsBucketName(rec.From)}, store); err != nil {
				return fmt.Errorf("no backups of %q in this project (database and its backup store not found)", rec.From)
			}
			return nil
		}
		return err
	}
	if source.Spec.Backups == nil {
		return fmt.Errorf("database %q has no backups (enable them with `shpyrd pg backups enable %s`)", rec.From, rec.From)
	}
	if source.Status.LastBackup == nil {
		return fmt.Errorf("database %q has no completed backup yet", rec.From)
	}
	if rec.TargetTime != nil && source.Status.RecoverableFrom != nil && rec.TargetTime.Time.Before(source.Status.RecoverableFrom.Time) {
		return fmt.Errorf("%s is before the earliest recoverable point of %q (%s)", rec.TargetTime.UTC().Format(time.RFC3339), rec.From, source.Status.RecoverableFrom.UTC().Format(time.RFC3339))
	}
	return nil
}

// backupStatus derives the archive's state from the completed Backups of
// the database (CloudNativePG fills the cluster's own summary fields late,
// or not at all, for plugin backups): the last one finished is LastBackup,
// the end of the oldest one is the earliest point in time recoverable.
func (r *PostgresReconciler) backupStatus(ctx context.Context, pg *shpyrdv1.Postgres, cluster *unstructured.Unstructured) {
	pg.Status.LastBackup, pg.Status.RecoverableFrom = nil, nil
	if pg.Spec.Backups == nil {
		return
	}
	var last, first *time.Time
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(CNPGBackupGVK.GroupVersion().WithKind("BackupList"))
	if err := r.List(ctx, list, client.InNamespace(pg.Namespace)); err == nil {
		for _, b := range list.Items {
			if cl, _, _ := unstructured.NestedString(b.Object, "spec", "cluster", "name"); cl != pg.Name {
				continue
			}
			if phase, _, _ := unstructured.NestedString(b.Object, "status", "phase"); phase != "completed" {
				continue
			}
			stopped, _, _ := unstructured.NestedString(b.Object, "status", "stoppedAt")
			t, err := time.Parse(time.RFC3339, stopped)
			if err != nil {
				continue
			}
			if last == nil || t.After(*last) {
				tt := t
				last = &tt
			}
			if first == nil || t.Before(*first) {
				tt := t
				first = &tt
			}
		}
	}
	// The cluster's own fields when it has them (they agree when present).
	if s, _, _ := unstructured.NestedString(cluster.Object, "status", "lastSuccessfulBackup"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil && (last == nil || t.After(*last)) {
			last = &t
		}
	}
	if s, _, _ := unstructured.NestedString(cluster.Object, "status", "firstRecoverabilityPoint"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil && (first == nil || t.Before(*first)) {
			first = &t
		}
	}
	if last != nil {
		pg.Status.LastBackup = &metav1.Time{Time: *last}
	}
	if first != nil {
		pg.Status.RecoverableFrom = &metav1.Time{Time: *first}
	}
}

func isNoKind(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no matches for kind")
}

var _ = corev1.SecretTypeOpaque
var _ client.Object = &shpyrdv1.ObjectBucket{}

// pendingError says a dependency is still being provisioned: the resource
// reports Provisioning and is retried soon, not Failed.
type pendingError struct{ msg string }

func (e *pendingError) Error() string { return e.msg }

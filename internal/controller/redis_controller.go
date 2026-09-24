package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// RedisReconciler runs a Redis-compatible store (Valkey by default) as a
// single-instance StatefulSet (RFC-0010): a generated password in a Secret,
// memory limits derived from the instance size, an optional volume for
// persistence. The Binder exposes REDIS_URL and friends.
type RedisReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	SystemNamespace string
	// Storage is the profile's disk rules (RFC-0060).
	Storage StorageProfile
}

// Engine images, pinned.
var redisImages = map[string]map[string]string{
	"valkey": {"8": "docker.io/valkey/valkey:8.1.10-alpine", "9": "docker.io/valkey/valkey:9.0.6-alpine"},
	"redis":  {"7": "docker.io/library/redis:7.4-alpine"},
}

const (
	RedisPort           = 6379
	defaultRedisStorage = "1Gi"
	redisUID            = 999 // the service user of both images
)

func (r *RedisReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&shpyrdv1.Redis{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

func (r *RedisReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	rd := &shpyrdv1.Redis{}
	if err := r.Get(ctx, req.NamespacedName, rd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rd.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := rd.DeepCopy()
	res, err := r.reconcile(ctx, rd)
	if apierrors.IsConflict(err) {
		return ctrl.Result{Requeue: true}, nil
	}
	if err != nil {
		logger.Error(err, "reconcile redis failed")
		rd.Status.Phase = shpyrdv1.ResourceFailed
		rd.Status.Message = err.Error()
		setResourceCondition(&rd.Status, rd.Generation, metav1.ConditionFalse, "Error", err.Error())
		r.Recorder.Event(rd, corev1.EventTypeWarning, "ReconcileError", err.Error())
		res = ctrl.Result{RequeueAfter: 30 * time.Second}
	}
	rd.Status.ObservedGeneration = rd.Generation
	if statusErr := r.Status().Patch(ctx, rd, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); statusErr != nil {
		if apierrors.IsConflict(statusErr) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, statusErr
	}
	return res, nil
}

func redisEngine(rd *shpyrdv1.Redis) (engine, version, image string, err error) {
	engine = firstNonEmpty(rd.Spec.Engine, "valkey")
	versions, ok := redisImages[engine]
	if !ok {
		return "", "", "", fmt.Errorf("unknown engine %q (valkey or redis)", engine)
	}
	version = rd.Spec.Version
	if version == "" {
		version = map[string]string{"valkey": "8", "redis": "7"}[engine]
	}
	image, ok = versions[version]
	if !ok {
		keys := make([]string, 0, len(versions))
		for k := range versions {
			keys = append(keys, k)
		}
		return "", "", "", fmt.Errorf("%s %s is not available (versions: %s)", engine, version, strings.Join(keys, ", "))
	}
	return engine, version, image, nil
}

// RedisSecretName holds the password and URL of a Redis.
func RedisSecretName(name string) string { return name + "-redis" }

func (r *RedisReconciler) reconcile(ctx context.Context, rd *shpyrdv1.Redis) (ctrl.Result, error) {
	engine, version, image, err := redisEngine(rd)
	if err != nil {
		return ctrl.Result{}, err
	}
	catalog := loadCatalog(ctx, r.Client, r.SystemNamespace)
	resources, _, err := catalog.Resolve(rd.Spec.Size, corev1.ResourceRequirements{})
	if err != nil {
		return ctrl.Result{}, err
	}

	// Password Secret, created once.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: RedisSecretName(rd.Name), Namespace: rd.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if len(sec.Data["password"]) == 0 {
			raw := make([]byte, 24)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			sec.Data = map[string][]byte{"password": []byte(hex.EncodeToString(raw))}
		}
		sec.Labels = mergeMaps(sec.Labels, map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", "shpyrd.io/redis": rd.Name})
		host := rd.Name
		sec.Data["host"] = []byte(host)
		sec.Data["port"] = []byte(fmt.Sprint(RedisPort))
		sec.Data["url"] = []byte(fmt.Sprintf("redis://:%s@%s:%d/0", sec.Data["password"], host, RedisPort))
		return controllerutil.SetControllerReference(rd, sec, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("credentials: %w", err)
	}

	// Service.
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: rd.Name, Namespace: rd.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = mergeMaps(svc.Labels, redisLabels(rd))
		svc.Spec.Selector = redisLabels(rd)
		svc.Spec.Ports = []corev1.ServicePort{{Name: "redis", Port: RedisPort, TargetPort: intstr.FromString("redis")}}
		return controllerutil.SetControllerReference(rd, svc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("service: %w", err)
	}

	// StatefulSet.
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: rd.Name, Namespace: rd.Namespace}}
	storage := resource.MustParse(defaultRedisStorage)
	if rd.Spec.Storage != nil {
		storage = *rd.Spec.Storage
	}
	// The provider minimum (RFC-0060): the disk is that size whatever was
	// asked; the status says so.
	rd.Status.Storage = ""
	if rounded, applied := r.Storage.Size(storage); applied && rd.Spec.Persistent {
		storage = rounded
		rd.Status.Storage = rounded.String()
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		if sts.ResourceVersion == "" { // new object
			sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: redisLabels(rd)}
			sts.Spec.ServiceName = rd.Name
			if rd.Spec.Persistent {
				claim := corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storage}},
					},
				}
				if r.Storage.Class != "" {
					claim.Spec.StorageClassName = ptr.To(r.Storage.Class)
				}
				sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{claim}
			}
		} else if rd.Spec.Persistent != (len(sts.Spec.VolumeClaimTemplates) > 0) {
			return fmt.Errorf("persistence cannot change after creation: delete the resource and create it again")
		}
		sts.Labels = mergeMaps(sts.Labels, redisLabels(rd))
		sts.Spec.Replicas = ptr.To[int32](1)
		sts.Spec.Template = r.podTemplate(rd, engine, image, resources)
		return controllerutil.SetControllerReference(rd, sts, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("statefulset: %w", err)
	}

	rd.Status.Endpoint = fmt.Sprintf("%s.%s.svc:%d", rd.Name, rd.Namespace, RedisPort)
	rd.Status.CredentialsSecret = sec.Name
	mode := "cache (data is lost on restart)"
	if rd.Spec.Persistent {
		mode = "persistent (" + storage.String() + ")"
	}
	if sts.Status.ReadyReplicas >= 1 && sts.Status.ObservedGeneration >= sts.Generation {
		rd.Status.Phase = shpyrdv1.ResourceReady
		rd.Status.Message = fmt.Sprintf("%s %s, %s", engine, version, mode)
		setResourceCondition(&rd.Status, rd.Generation, metav1.ConditionTrue, "Ready", rd.Status.Message)
		return ctrl.Result{}, nil
	}
	rd.Status.Phase = shpyrdv1.ResourceProvisioning
	rd.Status.Message = fmt.Sprintf("starting %s %s, %s", engine, version, mode)
	setResourceCondition(&rd.Status, rd.Generation, metav1.ConditionFalse, "Provisioning", rd.Status.Message)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func redisLabels(rd *shpyrdv1.Redis) map[string]string {
	return map[string]string{"app.kubernetes.io/name": "redis", "shpyrd.io/redis": rd.Name, shpyrdv1.LabelManagedBy: "shpyrd"}
}

// podTemplate runs the engine with a password, a memory ceiling at 75% of
// the allocation and LRU eviction in cache mode, AOF on the volume when
// persistent. Both images have the service user at uid 999.
func (r *RedisReconciler) podTemplate(rd *shpyrdv1.Redis, engine, image string, res corev1.ResourceRequirements) corev1.PodTemplateSpec {
	server := engine + "-server"
	maxmemory := int64(0)
	if mem, ok := res.Requests[corev1.ResourceMemory]; ok {
		maxmemory = mem.Value() * 3 / 4
	}
	args := []string{server, "--requirepass", "$(REDIS_PASSWORD)", "--protected-mode", "no"}
	if maxmemory > 0 {
		args = append(args, "--maxmemory", fmt.Sprint(maxmemory))
	}
	if rd.Spec.Persistent {
		args = append(args, "--appendonly", "yes", "--dir", "/data", "--maxmemory-policy", "noeviction")
	} else {
		args = append(args, "--save", "", "--maxmemory-policy", "allkeys-lru")
	}
	uid := ptr.To[int64](redisUID)
	container := corev1.Container{
		Name:  "redis",
		Image: image,
		Args:  args,
		Env: []corev1.EnvVar{{Name: "REDIS_PASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: RedisSecretName(rd.Name)}, Key: "password"},
		}}},
		Ports:     []corev1.ContainerPort{{Name: "redis", ContainerPort: RedisPort}},
		Resources: res,
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("redis")}},
			PeriodSeconds: 5,
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: uid, RunAsGroup: uid, RunAsNonRoot: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
	if rd.Spec.Persistent {
		container.VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: redisLabels(rd)},
		Spec: corev1.PodSpec{
			EnableServiceLinks: ptr.To(false),
			SecurityContext:    &corev1.PodSecurityContext{FSGroup: uid, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers:         []corev1.Container{container},
		},
	}
}

// ---- binding ----------------------------------------------------------------

// RedisBinder exposes a Redis as config vars.
type RedisBinder struct{}

// DefaultPrefix gives REDIS_URL and friends.
func (RedisBinder) DefaultPrefix() string { return "REDIS" }

// ConfigVars reads the generated credentials Secret.
func (RedisBinder) ConfigVars(ctx context.Context, c client.Client, namespace, name, prefix string) (map[string]string, error) {
	rd := &shpyrdv1.Redis{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, rd); err != nil {
		return nil, err
	}
	if rd.Status.Phase != shpyrdv1.ResourceReady {
		return nil, &NotReadyError{Msg: fmt.Sprintf("Redis %s is %s", name, strings.ToLower(firstNonEmpty(rd.Status.Phase, "provisioning")))}
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: RedisSecretName(name)}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &NotReadyError{Msg: fmt.Sprintf("Redis %s has no credentials yet", name)}
		}
		return nil, err
	}
	return map[string]string{
		prefix + "_URL":      string(sec.Data["url"]),
		prefix + "_HOST":     string(sec.Data["host"]),
		prefix + "_PORT":     string(sec.Data["port"]),
		prefix + "_PASSWORD": string(sec.Data["password"]),
	}, nil
}

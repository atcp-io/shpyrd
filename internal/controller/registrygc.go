package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// Garbage collection of the in-cluster registry (RFC-0059). Deleting a
// manifest only unlinks it; the blobs stay until `registry garbage-collect`
// runs, which needs the registry read-only. RegistryGC runs the sequence on
// a schedule and on demand: switch the registry to read-only (a copy of its
// configuration with maintenance.readonly enabled, swapped in with a
// Recreate rollout: pulls keep working, pushes fail and builds retry), run
// the collector in a Job on the registry's node against the same volume,
// switch back, record the outcome.

const (
	registryDeploymentName = "registry"
	registryClaimName      = "registry-data"
	registryConfigName     = "registry-config"
	registryGCRecordName   = "registry-gc"
	registryReadOnlyConfig = "registry-config-readonly"
	// DefaultRegistryGCSchedule is Sunday 04:00 UTC.
	DefaultRegistryGCSchedule = "0 4 * * 0"
)

// GCStatus is what the dashboard and the CLI show.
type GCStatus struct {
	Schedule       string     `json:"schedule"`
	NextRun        *time.Time `json:"nextRun,omitempty"`
	Running        bool       `json:"running"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	LastRun        *time.Time `json:"lastRun,omitempty"`
	LastResult     string     `json:"lastResult,omitempty"` // "ok" or the error
	LastDuration   string     `json:"lastDuration,omitempty"`
	ReclaimedBytes int64      `json:"reclaimedBytes"`
	UsedBytes      int64      `json:"usedBytes"` // after the last run
}

// RegistryGC schedules and runs collections. It is a manager Runnable that
// needs leader election so one replica acts.
type RegistryGC struct {
	Client client.Client
	// Reader bypasses the manager cache: the Jobs and Deployment state read
	// here must be current, not what an informer happens to hold.
	Reader    client.Reader
	Namespace string
	// Schedule is a cron expression (five fields, UTC); "" disables the
	// schedule (on-demand runs still work), "off" too.
	Schedule string
	// Image runs the collector: the registry image itself.
	Image  string
	Logger *slog.Logger

	mu      sync.Mutex
	running bool
	started time.Time
	sched   cron.Schedule
}

// NeedLeaderElection makes the manager start it on the leader only.
func (g *RegistryGC) NeedLeaderElection() bool { return true }

// Start runs the schedule until ctx is done.
func (g *RegistryGC) Start(ctx context.Context) error {
	if g.Logger == nil {
		g.Logger = slog.Default()
	}
	if g.Schedule != "" && g.Schedule != "off" {
		s, err := cron.ParseStandard(g.Schedule)
		if err != nil {
			return fmt.Errorf("registry GC schedule %q: %w", g.Schedule, err)
		}
		g.mu.Lock()
		g.sched = s
		g.mu.Unlock()
	}
	for {
		next := g.next(time.Now())
		if next == nil {
			<-ctx.Done()
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(*next)):
			if err := g.Trigger(ctx); err != nil && !errors.Is(err, errGCRunning) {
				g.Logger.Warn("registry garbage collection", "err", err)
			}
		}
	}
}

var errGCRunning = errors.New("a registry garbage collection is already running")

// Trigger starts a collection in the background; it returns errGCRunning
// when one is in progress.
func (g *RegistryGC) Trigger(ctx context.Context) error {
	g.mu.Lock()
	if g.running {
		g.mu.Unlock()
		return errGCRunning
	}
	g.running, g.started = true, time.Now()
	g.mu.Unlock()
	go func() {
		// Detached from the caller's request; bounded on its own.
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Hour)
		defer cancel()
		result, reclaimed, used, err := g.run(runCtx)
		g.mu.Lock()
		g.running = false
		started := g.started
		g.mu.Unlock()
		if err != nil {
			result = err.Error()
			g.Logger.Warn("registry garbage collection failed", "err", err)
		} else {
			g.Logger.Info("registry garbage collection done", "reclaimed", reclaimed, "used", used, "duration", time.Since(started).Truncate(time.Second).String())
		}
		g.record(runCtx, started, result, reclaimed, used)
	}()
	return nil
}

// Status combines the persisted record with the live state.
func (g *RegistryGC) Status(ctx context.Context) GCStatus {
	st := GCStatus{Schedule: g.Schedule}
	if g.Schedule == "off" {
		st.Schedule = ""
	}
	cm := &corev1.ConfigMap{}
	if err := g.reader().Get(ctx, types.NamespacedName{Namespace: g.Namespace, Name: registryGCRecordName}, cm); err == nil {
		if t, err := time.Parse(time.RFC3339, cm.Data["lastRun"]); err == nil {
			st.LastRun = &t
		}
		st.LastResult = cm.Data["lastResult"]
		st.LastDuration = cm.Data["lastDuration"]
		st.ReclaimedBytes, _ = strconv.ParseInt(cm.Data["reclaimedBytes"], 10, 64)
		st.UsedBytes, _ = strconv.ParseInt(cm.Data["usedBytes"], 10, 64)
	}
	g.mu.Lock()
	st.Running = g.running
	if g.running {
		s := g.started
		st.StartedAt = &s
	}
	g.mu.Unlock()
	st.NextRun = g.next(time.Now())
	return st
}

func (g *RegistryGC) reader() client.Reader {
	if g.Reader != nil {
		return g.Reader
	}
	return g.Client
}

func (g *RegistryGC) next(from time.Time) *time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sched == nil {
		return nil
	}
	n := g.sched.Next(from.UTC())
	return &n
}

// run performs one collection and returns the collector's summary.
func (g *RegistryGC) run(ctx context.Context) (result string, reclaimed, used int64, err error) {
	if err := g.setReadOnly(ctx, true); err != nil {
		return "", 0, 0, err
	}
	defer func() {
		// Always back to read-write, whatever happened; a failure here is
		// the worse outcome and must be seen.
		if rerr := g.setReadOnly(context.WithoutCancel(ctx), false); rerr != nil {
			g.Logger.Error("registry left read-only: fix with `shpyrd cluster init --only registry`", "err", rerr)
			if err == nil {
				err = rerr
			}
		}
	}()
	node, err := g.registryNode(ctx)
	if err != nil {
		return "", 0, 0, err
	}
	job := g.job(node)
	if err := g.Client.Create(ctx, job); err != nil {
		return "", 0, 0, fmt.Errorf("create collector job: %w", err)
	}
	defer func() {
		_ = g.Client.Delete(context.WithoutCancel(ctx), job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	}()
	summary, err := g.waitJob(ctx, job)
	if err != nil {
		return "", 0, 0, err
	}
	reclaimed = summary.Before - summary.After
	if reclaimed < 0 {
		reclaimed = 0
	}
	return "ok", reclaimed, summary.After, nil
}

// setReadOnly points the registry at the read-only copy of its configuration
// (or back at the original) and waits for the Recreate rollout so the
// collector never runs beside a writable registry.
func (g *RegistryGC) setReadOnly(ctx context.Context, on bool) error {
	if on {
		if err := g.writeReadOnlyConfig(ctx); err != nil {
			return err
		}
	}
	dep := &appsv1.Deployment{}
	if err := g.reader().Get(ctx, types.NamespacedName{Namespace: g.Namespace, Name: registryDeploymentName}, dep); err != nil {
		return fmt.Errorf("registry deployment: %w", err)
	}
	want := registryConfigName
	if on {
		want = registryReadOnlyConfig
	}
	changed := false
	for i := range dep.Spec.Template.Spec.Volumes {
		v := &dep.Spec.Template.Spec.Volumes[i]
		if v.Name == "config" && v.ConfigMap != nil && v.ConfigMap.Name != want {
			v.ConfigMap.Name = want
			changed = true
		}
	}
	if changed {
		if err := g.Client.Update(ctx, dep); err != nil {
			return fmt.Errorf("update registry deployment: %w", err)
		}
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if err := g.reader().Get(ctx, types.NamespacedName{Namespace: g.Namespace, Name: registryDeploymentName}, dep); err != nil {
			return err
		}
		if dep.Status.ObservedGeneration >= dep.Generation && dep.Status.UpdatedReplicas == 1 && dep.Status.ReadyReplicas == 1 && dep.Status.Replicas == 1 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("registry did not roll out within 5 minutes (read-only=%v)", on)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// writeReadOnlyConfig derives the read-only configuration from the live one
// so both stay identical in everything else.
func (g *RegistryGC) writeReadOnlyConfig(ctx context.Context) error {
	src := &corev1.ConfigMap{}
	if err := g.reader().Get(ctx, types.NamespacedName{Namespace: g.Namespace, Name: registryConfigName}, src); err != nil {
		return fmt.Errorf("registry config: %w", err)
	}
	var cfg map[string]interface{}
	if err := yaml.Unmarshal([]byte(src.Data["config.yml"]), &cfg); err != nil {
		return fmt.Errorf("registry config: %w", err)
	}
	storage, _ := cfg["storage"].(map[string]interface{})
	if storage == nil {
		return errors.New("registry config has no storage section")
	}
	maintenance, _ := storage["maintenance"].(map[string]interface{})
	if maintenance == nil {
		maintenance = map[string]interface{}{}
		storage["maintenance"] = maintenance
	}
	maintenance["readonly"] = map[string]interface{}{"enabled": true}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: registryReadOnlyConfig, Namespace: g.Namespace}}
	err = g.reader().Get(ctx, client.ObjectKeyFromObject(cm), cm)
	switch {
	case apierrors.IsNotFound(err):
		cm.Labels = map[string]string{"app.kubernetes.io/part-of": "shpyrd", "app.kubernetes.io/name": registryDeploymentName}
		cm.Data = map[string]string{"config.yml": string(out)}
		return g.Client.Create(ctx, cm)
	case err != nil:
		return err
	}
	cm.Data = map[string]string{"config.yml": string(out)}
	return g.Client.Update(ctx, cm)
}

// registryNode is the node the registry pod runs on: the collector must
// mount the same ReadWriteOnce volume.
func (g *RegistryGC) registryNode(ctx context.Context) (string, error) {
	var pods corev1.PodList
	if err := g.reader().List(ctx, &pods, client.InNamespace(g.Namespace), client.MatchingLabels{"app.kubernetes.io/name": registryDeploymentName}); err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" {
			return p.Spec.NodeName, nil
		}
	}
	return "", errors.New("no running registry pod")
}

// gcSummary is what the collector Job reports in its termination message.
type gcSummary struct {
	Before int64 `json:"before"`
	After  int64 `json:"after"`
}

func (g *RegistryGC) job(node string) *batchv1.Job {
	script := `set -e
before=$(du -sk /var/lib/registry | awk '{print $1 * 1024}')
registry garbage-collect --delete-untagged /etc/distribution/config.yml > /tmp/gc.log 2>&1 || { tail -n 20 /tmp/gc.log; exit 1; }
tail -n 3 /tmp/gc.log
after=$(du -sk /var/lib/registry | awk '{print $1 * 1024}')
printf '{"before":%s,"after":%s}' "$before" "$after" > /dev/termination-log
echo "reclaimed $((before - after)) bytes"`
	uid := int64(1000)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("registry-gc-%d", time.Now().Unix()),
			Namespace: g.Namespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "registry-gc", "app.kubernetes.io/part-of": "shpyrd"},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			ActiveDeadlineSeconds:   ptr.To[int64](2700),
			TTLSecondsAfterFinished: ptr.To[int32](3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "registry-gc"}},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeName:      node,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true), RunAsUser: &uid, FSGroup: &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    "gc",
						Image:   g.Image,
						Command: []string{"sh", "-c", script},
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "data", MountPath: "/var/lib/registry"},
							{Name: "config", MountPath: "/etc/distribution", ReadOnly: true},
						},
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					}},
					Volumes: []corev1.Volume{
						{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: registryClaimName}}},
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: registryConfigName}}}},
					},
				},
			},
		},
	}
}

// waitJob waits for the collector to finish and parses its summary.
func (g *RegistryGC) waitJob(ctx context.Context, job *batchv1.Job) (*gcSummary, error) {
	for {
		cur := &batchv1.Job{}
		if err := g.reader().Get(ctx, client.ObjectKeyFromObject(job), cur); err != nil {
			return nil, err
		}
		if cur.Status.Succeeded > 0 {
			return g.summary(ctx, job)
		}
		if cur.Status.Failed > 0 {
			msg := "collector failed"
			if s, err := g.summaryMessage(ctx, job); err == nil && s != "" {
				msg += ": " + s
			}
			return nil, errors.New(msg)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (g *RegistryGC) summaryMessage(ctx context.Context, job *batchv1.Job) (string, error) {
	var pods corev1.PodList
	if err := g.reader().List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Terminated != nil {
				return cs.State.Terminated.Message, nil
			}
		}
	}
	return "", errors.New("no terminated container")
}

func (g *RegistryGC) summary(ctx context.Context, job *batchv1.Job) (*gcSummary, error) {
	msg, err := g.summaryMessage(ctx, job)
	if err != nil {
		return nil, err
	}
	var s gcSummary
	if err := json.Unmarshal([]byte(msg), &s); err != nil {
		return nil, fmt.Errorf("collector summary: %s", shortMessage(msg))
	}
	return &s, nil
}

// record persists the outcome for Status.
func (g *RegistryGC) record(ctx context.Context, started time.Time, result string, reclaimed, used int64) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: registryGCRecordName, Namespace: g.Namespace}}
	data := map[string]string{
		"lastRun":        started.UTC().Format(time.RFC3339),
		"lastResult":     result,
		"lastDuration":   time.Since(started).Truncate(time.Second).String(),
		"reclaimedBytes": strconv.FormatInt(reclaimed, 10),
		"usedBytes":      strconv.FormatInt(used, 10),
	}
	err := g.reader().Get(ctx, client.ObjectKeyFromObject(cm), cm)
	switch {
	case apierrors.IsNotFound(err):
		cm.Labels = map[string]string{"app.kubernetes.io/part-of": "shpyrd"}
		cm.Data = data
		err = g.Client.Create(ctx, cm)
	case err == nil:
		cm.Data = data
		err = g.Client.Update(ctx, cm)
	}
	if err != nil {
		g.Logger.Warn("record registry GC outcome", "err", err)
	}
}

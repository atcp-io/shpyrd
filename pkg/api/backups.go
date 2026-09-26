package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shpyrd-io/shpyrd/pkg/backup"
	"github.com/shpyrd-io/shpyrd/pkg/install"
)

// Platform backups (RFC-0037): the CronJob of the platform-backup component
// writes encrypted archives to the provider's bucket; this is the card and
// the "back up now" button. The passphrase never leaves the cluster through
// the API (`shpyrd cluster backup key` reads the Secret).

const backupCronJobName = "platform-backup"

// BackupInfo is GET /api/cluster/backups.
type BackupInfo struct {
	Enabled  bool   `json:"enabled"`
	Target   string `json:"target,omitempty"` // s3://bucket/prefix
	Endpoint string `json:"endpoint,omitempty"`
	Schedule string `json:"schedule,omitempty"` // cron, UTC
	Keep     int    `json:"keep,omitempty"`
	// Static access key stored for the target; false means the pod's cloud
	// identity signs.
	AccessKey      bool           `json:"accessKey"`
	LastScheduled  *time.Time     `json:"lastScheduled,omitempty"`
	LastSuccessful *time.Time     `json:"lastSuccessful,omitempty"`
	Runs           []BackupRun    `json:"runs"`
	Archives       []backup.Entry `json:"archives"`
	// Error says why the archives could not be listed (credentials, network).
	Error string `json:"error,omitempty"`
}

// BackupRun is one Job of the CronJob (scheduled or `shpyrd cluster backup`).
type BackupRun struct {
	Name     string     `json:"name"`
	Status   string     `json:"status"` // running, succeeded, failed
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	Message  string     `json:"message,omitempty"`
}

// backupTarget reads the target the hook stored; nil when the component is
// not installed.
func (s *Server) backupTarget(ctx context.Context) (*backup.Target, *corev1.Secret, error) {
	sec, err := s.kube.Kube.CoreV1().Secrets(s.kube.Namespace).Get(ctx, install.BackupTargetSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	uri := string(sec.Data["SHPYRD_BACKUP_TARGET"])
	bucket, prefix, err := backup.ParseURI(uri)
	if err != nil {
		return nil, sec, err
	}
	return &backup.Target{
		Bucket: bucket, Prefix: prefix,
		Endpoint:  string(sec.Data["SHPYRD_BACKUP_ENDPOINT"]),
		Region:    string(sec.Data["SHPYRD_BACKUP_REGION"]),
		AccessKey: string(sec.Data["AWS_ACCESS_KEY_ID"]),
		SecretKey: string(sec.Data["AWS_SECRET_ACCESS_KEY"]),
	}, sec, nil
}

func (s *Server) listBackups(c *gin.Context) {
	ctx := c.Request.Context()
	out := BackupInfo{Runs: []BackupRun{}, Archives: []backup.Entry{}}
	target, sec, err := s.backupTarget(ctx)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	if target == nil {
		c.JSON(http.StatusOK, out)
		return
	}
	out.Enabled = true
	out.Target = string(sec.Data["SHPYRD_BACKUP_TARGET"])
	out.Endpoint = target.Endpoint
	out.AccessKey = target.AccessKey != ""

	if cj, err := s.kube.Kube.BatchV1().CronJobs(s.kube.Namespace).Get(ctx, backupCronJobName, metav1.GetOptions{}); err == nil {
		out.Schedule = cj.Spec.Schedule
		if cj.Status.LastScheduleTime != nil {
			t := cj.Status.LastScheduleTime.Time
			out.LastScheduled = &t
		}
		if cj.Status.LastSuccessfulTime != nil {
			t := cj.Status.LastSuccessfulTime.Time
			out.LastSuccessful = &t
		}
		for _, ctr := range cj.Spec.JobTemplate.Spec.Template.Spec.Containers {
			for _, env := range ctr.Env {
				if env.Name == "SHPYRD_BACKUP_KEEP" {
					out.Keep, _ = strconv.Atoi(env.Value)
				}
			}
		}
	}
	if jobs, err := s.kube.Kube.BatchV1().Jobs(s.kube.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=platform-backup"}); err == nil {
		for _, j := range jobs.Items {
			out.Runs = append(out.Runs, backupRun(j))
		}
		sort.Slice(out.Runs, func(i, k int) bool {
			ti, tk := time.Time{}, time.Time{}
			if out.Runs[i].Started != nil {
				ti = *out.Runs[i].Started
			}
			if out.Runs[k].Started != nil {
				tk = *out.Runs[k].Started
			}
			return ti.After(tk)
		})
	}

	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	entries, err := target.List(lctx)
	if err != nil {
		out.Error = err.Error()
	} else {
		out.Archives = entries
	}
	// The CronJob only counts the runs it scheduled; manual runs and the
	// archives themselves say when the last good backup was.
	for _, r := range out.Runs {
		if r.Status == "succeeded" && r.Finished != nil && (out.LastSuccessful == nil || r.Finished.After(*out.LastSuccessful)) {
			t := *r.Finished
			out.LastSuccessful = &t
		}
	}
	if len(out.Archives) > 0 && (out.LastSuccessful == nil || out.Archives[0].Modified.After(*out.LastSuccessful)) {
		t := out.Archives[0].Modified
		out.LastSuccessful = &t
	}
	c.JSON(http.StatusOK, out)
}

func backupRun(j batchv1.Job) BackupRun {
	r := BackupRun{Name: j.Name, Status: "running"}
	if j.Status.StartTime != nil {
		t := j.Status.StartTime.Time
		r.Started = &t
	} else {
		t := j.CreationTimestamp.Time
		r.Started = &t
	}
	if j.Status.CompletionTime != nil {
		t := j.Status.CompletionTime.Time
		r.Finished = &t
	}
	for _, cond := range j.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete, batchv1.JobSuccessCriteriaMet:
			r.Status = "succeeded"
		case batchv1.JobFailed:
			r.Status = "failed"
			r.Message = cond.Message
			if cond.LastTransitionTime.Time != (time.Time{}) {
				t := cond.LastTransitionTime.Time
				r.Finished = &t
			}
		}
	}
	return r
}

// runBackup is POST /api/cluster/backups: one Job from the CronJob's
// template, now.
func (s *Server) runBackup(c *gin.Context) {
	ctx := c.Request.Context()
	cj, err := s.kube.Kube.BatchV1().CronJobs(s.kube.Namespace).Get(ctx, backupCronJobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		abort(c, http.StatusNotImplemented, errors.New("platform backups are not set up: `shpyrd cluster init --backup-target s3://…`"))
		return
	}
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	if jobs, err := s.kube.Kube.BatchV1().Jobs(s.kube.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=platform-backup"}); err == nil {
		for _, j := range jobs.Items {
			if backupRun(j).Status == "running" {
				abort(c, http.StatusConflict, fmt.Errorf("backup %s is running", j.Name))
				return
			}
		}
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("%s-manual-%d", backupCronJobName, time.Now().Unix()),
			Namespace:   s.kube.Namespace,
			Labels:      map[string]string{"app.kubernetes.io/name": "platform-backup", "app.kubernetes.io/part-of": "shpyrd", "shpyrd.io/trigger": "manual"},
			Annotations: map[string]string{"cronjob.kubernetes.io/instantiate": "manual"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "CronJob", Name: cj.Name, UID: cj.UID,
			}},
		},
		Spec: *cj.Spec.JobTemplate.Spec.DeepCopy(),
	}
	created, err := s.kube.Kube.BatchV1().Jobs(s.kube.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	s.audit(c, "", "backup.run", created.Name, "platform backup started")
	c.JSON(http.StatusAccepted, gin.H{"job": created.Name, "status": "running"})
}

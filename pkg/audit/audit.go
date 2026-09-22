// Package audit records who did what (RFC-0008) as Kubernetes Events with
// structured annotations: the API records its mutations, the CLI records
// the actions it performs directly on the cluster. Events are the durable
// store until a storage extension provides a better one; their retention is
// the API server's event TTL (an hour by default).
package audit

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Reason marks audit Events.
const Reason = "Audit"

// Annotations carrying the structured fields.
const (
	AnnotationActor  = "shpyrd.io/actor"
	AnnotationAction = "shpyrd.io/action"
	AnnotationTarget = "shpyrd.io/target"
	AnnotationDetail = "shpyrd.io/detail"
	AnnotationFrom   = "shpyrd.io/from"
	AnnotationVia    = "shpyrd.io/via"
)

// Entry is one audited action.
type Entry struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
	From   string    `json:"from,omitempty"`
	Via    string    `json:"via"` // api, cli
}

// Ref is the object an entry is attached to.
type Ref struct {
	Namespace string
	Kind      string
	Name      string
	UID       string
}

// AppRef points at the App of a project.
func AppRef(project string) Ref {
	return Ref{Namespace: "app-" + project, Kind: "App", Name: project}
}

// ClusterRef points at the install record for cluster-level actions.
func ClusterRef(systemNS string) Ref {
	return Ref{Namespace: systemNS, Kind: "ConfigMap", Name: "shpyrd-install"}
}

// Record writes an audit Event. Failures are returned but callers usually
// only log them: auditing must not break the action.
func Record(ctx context.Context, k kubernetes.Interface, ref Ref, e Entry) error {
	if k == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	msg := fmt.Sprintf("%s %s", e.Action, e.Target)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	msg += " by " + e.Actor
	if e.From != "" {
		msg += " from " + e.From
	}
	ts := metav1.NewTime(e.Time)
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "audit-",
			Namespace:    ref.Namespace,
			Annotations: map[string]string{
				AnnotationActor: e.Actor, AnnotationAction: e.Action, AnnotationTarget: e.Target,
				AnnotationDetail: e.Detail, AnnotationFrom: e.From, AnnotationVia: e.Via,
			},
		},
		InvolvedObject:      corev1.ObjectReference{Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name, APIVersion: apiVersionFor(ref.Kind)},
		Reason:              Reason,
		Message:             msg,
		Type:                corev1.EventTypeNormal,
		Source:              corev1.EventSource{Component: "shpyrd-" + e.Via},
		FirstTimestamp:      ts,
		LastTimestamp:       ts,
		Count:               1,
		ReportingController: "shpyrd.io/" + e.Via,
		ReportingInstance:   e.Via,
	}
	_, err := k.CoreV1().Events(ref.Namespace).Create(ctx, ev, metav1.CreateOptions{})
	return err
}

func apiVersionFor(kind string) string {
	switch kind {
	case "App", "Volume":
		return "shpyrd.io/v1alpha1"
	}
	return "v1"
}

// List returns the audit entries attached to ref, newest first.
func List(ctx context.Context, k kubernetes.Interface, ref Ref, limit int) ([]Entry, error) {
	list, err := k.CoreV1().Events(ref.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "reason=" + Reason + ",involvedObject.name=" + ref.Name + ",involvedObject.kind=" + ref.Kind,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(list.Items))
	for _, ev := range list.Items {
		a := ev.Annotations
		t := ev.LastTimestamp.Time
		if t.IsZero() {
			t = ev.CreationTimestamp.Time
		}
		out = append(out, Entry{
			Time: t, Actor: a[AnnotationActor], Action: a[AnnotationAction], Target: a[AnnotationTarget],
			Detail: a[AnnotationDetail], From: a[AnnotationFrom], Via: a[AnnotationVia],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LocalActor names the person running the CLI: user@host.
func LocalActor() string {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, _ := os.Hostname()
	if host == "" {
		return name
	}
	return name + "@" + strings.SplitN(host, ".", 2)[0]
}

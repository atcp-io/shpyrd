package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// One-off instances (`shpyrd run`) are plain pods labelled
// shpyrd.io/process=run. The CLI deletes them when the session ends; this
// keeps the namespace clean when it cannot (killed CLI, --detach, deadline).

// runProcess is the process label of one-off pods.
const runProcess = "run"

// runPodRetention is how long finished one-off pods are kept so their
// output stays readable through `shpyrd logs --process run`.
const runPodRetention = 10 * time.Minute

// isRunPod filters the pod watch to one-off instances.
var isRunPod = predicate.NewPredicateFuncs(func(o client.Object) bool {
	return o.GetLabels()[shpyrdv1.LabelProcess] == runProcess
})

// runPodToApp maps a one-off pod to its App.
func runPodToApp(_ context.Context, obj client.Object) []reconcile.Request {
	app := obj.GetLabels()[shpyrdv1.LabelApp]
	if app == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: app}}}
}

// gcRunPods deletes finished one-off pods once they are older than the
// retention period and returns how long to wait before checking again (0
// when nothing is pending).
func (r *AppReconciler) gcRunPods(ctx context.Context, app *shpyrdv1.App) (time.Duration, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(app.Namespace),
		client.MatchingLabels{shpyrdv1.LabelApp: app.Name, shpyrdv1.LabelProcess: runProcess}); err != nil {
		return 0, err
	}
	var next time.Duration
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		finished := podFinishedAt(p)
		if finished.IsZero() {
			continue
		}
		if wait := time.Until(finished.Add(runPodRetention)); wait > 0 {
			if next == 0 || wait < next {
				next = wait
			}
			continue
		}
		if err := r.Delete(ctx, p); client.IgnoreNotFound(err) != nil {
			return 0, err
		}
	}
	return next, nil
}

// podFinishedAt returns when a pod finished, or zero while it is running.
func podFinishedAt(p *corev1.Pod) time.Time {
	if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
		return time.Time{}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && !t.FinishedAt.IsZero() {
			return t.FinishedAt.Time
		}
	}
	// Pods killed by activeDeadlineSeconds may carry no container timestamps.
	return p.CreationTimestamp.Time
}

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// Container waiting reasons that mean an instance will not come up on its
// own. Kubernetes keeps retrying; the user needs to know why.
var failingReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"InvalidImageName":           true,
	"RunContainerError":          true,
}

// processHealth inspects the pods of one process type and reports how many
// instances are failing and a human readable reason for the first one.
func (r *AppReconciler) processHealth(ctx context.Context, app *shpyrdv1.App, process string) (failing int32, reason string) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(app.Namespace), client.MatchingLabels{shpyrdv1.LabelApp: app.Name, shpyrdv1.LabelProcess: process}); err != nil {
		return 0, ""
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != "app" {
				continue
			}
			var why string
			switch {
			case cs.State.Waiting != nil && failingReasons[cs.State.Waiting.Reason]:
				why = cs.State.Waiting.Reason
				if m := cs.State.Waiting.Message; strings.Contains(m, "runAsNonRoot") {
					// Pod security: processes run as non-root (RFC-0008), and
					// the kubelet can only verify a numeric user.
					switch {
					case strings.Contains(m, "non-numeric user"):
						why = "the image sets a named USER, which Kubernetes cannot verify as non-root: use a numeric USER in the Dockerfile (for example USER 1000)"
					default:
						why = "the image runs as root: shpyrd runs processes as a non-root user (add USER 1000 to the Dockerfile)"
					}
					break
				}
				if t := cs.LastTerminationState.Terminated; t != nil {
					why += fmt.Sprintf(" (exit %d)", t.ExitCode)
					if m := shortMessage(t.Message); m != "" {
						why += ": " + m
					}
				} else if m := shortMessage(cs.State.Waiting.Message); m != "" && !strings.HasPrefix(m, "back-off") {
					why += ": " + m
				}
			case cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 && pod.Status.Phase == corev1.PodFailed:
				why = fmt.Sprintf("exited with code %d", cs.State.Terminated.ExitCode)
				if m := shortMessage(cs.State.Terminated.Message); m != "" {
					why += ": " + m
				}
			}
			if why != "" {
				failing++
				if reason == "" {
					reason = why
				}
			}
		}
	}
	return failing, reason
}

// shortMessage trims runtime noise from container error messages.
func shortMessage(m string) string {
	m = strings.TrimSpace(m)
	// containerd/runc prefix chains end with the interesting part.
	if i := strings.LastIndex(m, "error during container init: "); i >= 0 {
		m = m[i+len("error during container init: "):]
	}
	if i := strings.LastIndex(m, "OCI runtime create failed: "); i >= 0 {
		m = m[i+len("OCI runtime create failed: "):]
	}
	if len(m) > 160 {
		m = m[:160] + "..."
	}
	return m
}

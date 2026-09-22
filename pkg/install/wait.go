package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"

	"shpyrd/pkg/kube"
)

const pollInterval = 2 * time.Second

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// waitFor blocks until spec is satisfied or ctx is done.
func (a *applier) waitFor(ctx context.Context, spec WaitSpec, defaultNS string) error {
	switch {
	case spec.Deployment != "":
		ns, name := splitNSName(spec.Deployment, defaultNS)
		return waitDeployment(ctx, a.kube, ns, name)
	case spec.StatefulSet != "":
		ns, name := splitNSName(spec.StatefulSet, defaultNS)
		return waitStatefulSet(ctx, a.kube, ns, name)
	case spec.DaemonSet != "":
		ns, name := splitNSName(spec.DaemonSet, defaultNS)
		return waitDaemonSet(ctx, a.kube, ns, name)
	case spec.Job != "":
		ns, name := splitNSName(spec.Job, defaultNS)
		return waitJob(ctx, a.kube, ns, name)
	case spec.CRD != "":
		return waitCRDEstablished(ctx, a.kube, spec.CRD)
	case spec.Endpoints != "":
		ns, name := splitNSName(spec.Endpoints, defaultNS)
		return waitEndpoints(ctx, a.kube, ns, name)
	case spec.DryRun != nil:
		return a.waitDryRun(ctx, spec.DryRun, defaultNS)
	case spec.Condition != nil:
		return a.waitCondition(ctx, spec.Condition, defaultNS)
	}
	return fmt.Errorf("empty wait spec")
}

func splitNSName(s, defaultNS string) (string, string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return defaultNS, s
}

// poll runs cond every pollInterval until it returns true, an error, or ctx
// ends. NotFound errors are treated as "not yet".
func poll(ctx context.Context, what string, cond func(context.Context) (bool, error)) error {
	err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		done, err := cond(ctx)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return done, err
	})
	if err != nil {
		return fmt.Errorf("waiting for %s: %w", what, err)
	}
	return nil
}

func waitDeployment(ctx context.Context, k *kube.Client, ns, name string) error {
	return poll(ctx, "deployment "+ns+"/"+name, func(ctx context.Context) (bool, error) {
		d, err := k.Kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		return d.Status.ObservedGeneration >= d.Generation &&
			d.Status.UpdatedReplicas >= want &&
			d.Status.AvailableReplicas >= want, nil
	})
}

func waitStatefulSet(ctx context.Context, k *kube.Client, ns, name string) error {
	return poll(ctx, "statefulset "+ns+"/"+name, func(ctx context.Context) (bool, error) {
		s, err := k.Kube.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		want := int32(1)
		if s.Spec.Replicas != nil {
			want = *s.Spec.Replicas
		}
		return s.Status.ObservedGeneration >= s.Generation &&
			s.Status.ReadyReplicas >= want &&
			s.Status.UpdateRevision == s.Status.CurrentRevision, nil
	})
}

func waitDaemonSet(ctx context.Context, k *kube.Client, ns, name string) error {
	return poll(ctx, "daemonset "+ns+"/"+name, func(ctx context.Context) (bool, error) {
		d, err := k.Kube.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return d.Status.ObservedGeneration >= d.Generation &&
			d.Status.NumberReady >= d.Status.DesiredNumberScheduled &&
			d.Status.UpdatedNumberScheduled >= d.Status.DesiredNumberScheduled, nil
	})
}

func waitJob(ctx context.Context, k *kube.Client, ns, name string) error {
	return poll(ctx, "job "+ns+"/"+name, func(ctx context.Context) (bool, error) {
		j, err := k.Kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, c := range j.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				return true, nil
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				return false, fmt.Errorf("job failed: %s", c.Message)
			}
		}
		return false, nil
	})
}

func waitCRDEstablished(ctx context.Context, k *kube.Client, name string) error {
	return poll(ctx, "crd "+name, func(ctx context.Context) (bool, error) {
		crd, err := k.Dynamic.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
		for _, c := range conds {
			m, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			if m["type"] == "Established" && m["status"] == "True" {
				return true, nil
			}
		}
		return false, nil
	})
}

// waitEndpoints waits until the service has at least one ready endpoint.
func waitEndpoints(ctx context.Context, k *kube.Client, ns, svc string) error {
	return poll(ctx, "endpoints "+ns+"/"+svc, func(ctx context.Context) (bool, error) {
		slices, err := k.Kube.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
			LabelSelector: "kubernetes.io/service-name=" + svc,
		})
		if err != nil {
			return false, err
		}
		for _, s := range slices.Items {
			for _, ep := range s.Endpoints {
				if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
					return true, nil
				}
			}
		}
		return false, nil
	})
}

// waitDryRun server-side dry-runs obj until the API server and its admission
// webhooks accept it. It is the reliable way to know a webhook is serving.
func (a *applier) waitDryRun(ctx context.Context, obj map[string]interface{}, defaultNS string) error {
	u := &unstructured.Unstructured{Object: obj}
	return poll(ctx, "dry-run "+u.GetKind()+" "+u.GetName(), func(ctx context.Context) (bool, error) {
		err := a.applyOne(ctx, u.DeepCopy(), defaultNS, true)
		if err == nil {
			return true, nil
		}
		if retryable(err) {
			a.log.Debug("dry-run not accepted yet", "kind", u.GetKind(), "err", err.Error())
			a.kube.InvalidateCache()
			return false, nil
		}
		return false, err
	})
}

// waitCondition waits for status.conditions[type].status == True on any
// object, resolving the resource through discovery.
func (a *applier) waitCondition(ctx context.Context, c *ConditionSpec, defaultNS string) error {
	condType := c.Type
	if condType == "" {
		condType = "Ready"
	}
	probe := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": c.APIVersion,
		"kind":       c.Kind,
		"metadata":   map[string]interface{}{"name": c.Name, "namespace": c.Namespace},
	}}
	what := fmt.Sprintf("%s %s %s", c.Kind, c.Name, condType)
	return poll(ctx, what, func(ctx context.Context) (bool, error) {
		ri, err := a.resourceFor(probe.DeepCopy(), defaultNS)
		if err != nil {
			if meta.IsNoMatchError(err) {
				a.kube.InvalidateCache()
				return false, nil
			}
			return false, err
		}
		obj, err := ri.Get(ctx, c.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		// A stale Ready=True from a previous spec does not count: wait until
		// the controller has observed the current generation.
		if observed, found, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration"); found && observed < obj.GetGeneration() {
			return false, nil
		}
		conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		for _, raw := range conds {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if m["type"] == condType {
				if m["status"] == "True" {
					return true, nil
				}
				if msg, _ := m["message"].(string); msg != "" {
					a.log.Debug("condition not met", "object", what, "reason", m["reason"], "message", msg)
				}
			}
		}
		return false, nil
	})
}

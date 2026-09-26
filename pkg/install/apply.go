package install

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"

	"github.com/shpyrd-io/shpyrd/pkg/kube"
)

const fieldManager = "shpyrd"

// installOrder mirrors Helm's ordering so dependencies exist before their
// dependents. Kinds not listed (custom resources) go after the listed ones;
// admission webhooks go last so they cannot block their own installation.
var installOrder = []string{
	"Namespace",
	"NetworkPolicy",
	"ResourceQuota",
	"LimitRange",
	"PodDisruptionBudget",
	"ServiceAccount",
	"Secret",
	"ConfigMap",
	"StorageClass",
	"PersistentVolume",
	"PersistentVolumeClaim",
	"CustomResourceDefinition",
	"ClusterRole",
	"ClusterRoleBinding",
	"Role",
	"RoleBinding",
	"Service",
	"DaemonSet",
	"Pod",
	"ReplicaSet",
	"Deployment",
	"HorizontalPodAutoscaler",
	"StatefulSet",
	"Job",
	"CronJob",
	"IngressClass",
	"Ingress",
	"APIService",
}

var lastKinds = map[string]bool{
	"MutatingWebhookConfiguration":   true,
	"ValidatingWebhookConfiguration": true,
}

func kindRank(kind string) int {
	for i, k := range installOrder {
		if k == kind {
			return i
		}
	}
	if lastKinds[kind] {
		return len(installOrder) + 1000
	}
	return len(installOrder) + 1
}

// sortObjects orders objects for application; the sort is stable so the
// manifest order is kept within a kind.
func sortObjects(objs []*unstructured.Unstructured) {
	sort.SliceStable(objs, func(i, j int) bool {
		return kindRank(objs[i].GetKind()) < kindRank(objs[j].GetKind())
	})
}

// applier performs server-side applies with retries for kinds whose CRD or
// admission webhook is not ready yet.
type applier struct {
	kube *kube.Client
	log  *slog.Logger
}

// applyAll applies objs. Namespaced objects without a namespace are placed in
// defaultNS. CRDs are applied first and waited for so custom resources from
// the same set can follow.
func (a *applier) applyAll(ctx context.Context, objs []*unstructured.Unstructured, defaultNS string) error {
	sortObjects(objs)

	var crds, rest []*unstructured.Unstructured
	for _, o := range objs {
		if o.GetKind() == "CustomResourceDefinition" {
			crds = append(crds, o)
		} else {
			rest = append(rest, o)
		}
	}

	if len(crds) > 0 {
		for _, o := range crds {
			if err := a.applyWithRetry(ctx, o, defaultNS); err != nil {
				return err
			}
		}
		for _, o := range crds {
			if err := waitCRDEstablished(ctx, a.kube, o.GetName()); err != nil {
				return err
			}
		}
		a.kube.InvalidateCache()
	}

	for _, o := range rest {
		if err := a.applyWithRetry(ctx, o, defaultNS); err != nil {
			return err
		}
	}
	return nil
}

// applyWithRetry retries while the kind is unknown (CRD still registering)
// or an admission webhook is not reachable yet.
func (a *applier) applyWithRetry(ctx context.Context, o *unstructured.Unstructured, defaultNS string) error {
	backoff := 2 * time.Second
	for {
		err := a.applyOne(ctx, o, defaultNS, false)
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return fmt.Errorf("apply %s %s: %w", o.GetKind(), qualifiedName(o), err)
		}
		a.log.Debug("apply not ready, retrying", "kind", o.GetKind(), "name", qualifiedName(o), "err", err.Error())
		a.kube.InvalidateCache()
		select {
		case <-ctx.Done():
			return fmt.Errorf("apply %s %s: %w (last error: %v)", o.GetKind(), qualifiedName(o), ctx.Err(), err)
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff += 2 * time.Second
		}
	}
}

// applyOne performs one server-side apply. With dryRun the request is
// validated by the API server and admission webhooks but not persisted.
func (a *applier) applyOne(ctx context.Context, o *unstructured.Unstructured, defaultNS string, dryRun bool) error {
	return a.applyOneAs(ctx, o, defaultNS, dryRun, fieldManager)
}

// applyOneAs is applyOne with an explicit field manager. Server-side apply
// makes a manager own exactly the fields it sends, so writers that share an
// object (the install record ConfigMap) must use distinct managers.
func (a *applier) applyOneAs(ctx context.Context, o *unstructured.Unstructured, defaultNS string, dryRun bool, manager string) error {
	ri, err := a.resourceFor(o, defaultNS)
	if err != nil {
		return err
	}
	// Fields that are server owned or meaningless in an apply request.
	unstructured.RemoveNestedField(o.Object, "status")
	unstructured.RemoveNestedField(o.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(o.Object, "metadata", "managedFields")
	data, err := json.Marshal(o.Object)
	if err != nil {
		return err
	}
	opts := metav1.PatchOptions{FieldManager: manager, Force: ptr.To(true)}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	_, err = ri.Patch(ctx, o.GetName(), types.ApplyPatchType, data, opts)
	return err
}

// resourceFor resolves the dynamic client for o, filling in the namespace of
// namespaced objects when missing.
func (a *applier) resourceFor(o *unstructured.Unstructured, defaultNS string) (dynamic.ResourceInterface, error) {
	gvk := o.GroupVersionKind()
	mapping, err := a.kube.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	res := a.kube.Dynamic.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := o.GetNamespace()
		if ns == "" {
			ns = defaultNS
			o.SetNamespace(ns)
		}
		return res.Namespace(ns), nil
	}
	return res, nil
}

func retryable(err error) bool {
	if meta.IsNoMatchError(err) {
		return true
	}
	if apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{
		"failed calling webhook",
		"connection refused",
		"no endpoints available",
		"the server is currently unable to handle the request",
		"context deadline exceeded",
		"EOF",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	// Some validating webhooks reject while their CA bundle is still being
	// injected; treat internal errors from webhooks as transient.
	return apierrors.IsInternalError(err) && strings.Contains(msg, "webhook")
}

func qualifiedName(o *unstructured.Unstructured) string {
	if ns := o.GetNamespace(); ns != "" {
		return ns + "/" + o.GetName()
	}
	return o.GetName()
}

// deleteAll removes objects in reverse apply order, ignoring the ones that
// are already gone. Cluster-scoped objects such as CRDs go last so nothing
// depends on them when they disappear.
func (a *applier) deleteAll(ctx context.Context, objs []*unstructured.Unstructured, defaultNS string) error {
	sortObjects(objs)
	for i := len(objs) - 1; i >= 0; i-- {
		o := objs[i]
		ri, err := a.resourceFor(o, defaultNS)
		if err != nil {
			if meta.IsNoMatchError(err) {
				continue // its CRD is gone already
			}
			return err
		}
		policy := metav1.DeletePropagationBackground
		if err := ri.Delete(ctx, o.GetName(), metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return nil
}

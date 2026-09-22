package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/sizes"
)

// AppReconciler turns an App into a kpack Image plus one Deployment (and
// Service) per process type and an Ingress for the web process.
type AppReconciler struct {
	client.Client
	// APIReader bypasses the cache for one-off reads (kpack Builds).
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	Config    Config
}

// SetupWithManager registers the controller and its watches.
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Config = r.Config.Defaults()
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	kpackImage := &unstructured.Unstructured{}
	kpackImage.SetGroupVersionKind(KpackImageGVK)

	return ctrl.NewControllerManagedBy(mgr).
		Named("app").
		For(&shpyrdv1.App{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Owns(kpackImage).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(envSecretToApp)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.sizesToAllApps)).
		Complete(r)
}

// sizesToAllApps requeues every App when the size catalog changes so their
// Deployments pick up new allocations.
func (r *AppReconciler) sizesToAllApps(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != sizes.ConfigMapName || obj.GetNamespace() != r.Config.SystemNamespace {
		return nil
	}
	var apps shpyrdv1.AppList
	if err := r.List(ctx, &apps); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(apps.Items))
	for _, a := range apps.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
	}
	return out
}

// catalog loads the size catalog from the cluster, falling back to the
// built-in defaults when it is missing or invalid.
func (r *AppReconciler) catalog(ctx context.Context) sizes.Catalog {
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Config.SystemNamespace, Name: sizes.ConfigMapName}, cm); err != nil {
		return sizes.Defaults()
	}
	c, err := sizes.Parse([]byte(cm.Data[sizes.ConfigMapKey]))
	if err != nil {
		log.FromContext(ctx).Info("size catalog invalid, using defaults", "err", err.Error())
		return sizes.Defaults()
	}
	return *c
}

// envSecretToApp maps Secret <app>-env to its App in the same namespace.
func envSecretToApp(_ context.Context, obj client.Object) []reconcile.Request {
	name, ok := strings.CutSuffix(obj.GetName(), shpyrdv1.EnvSecretSuffix)
	if !ok || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// Reconcile implements the App state machine:
//
//	Pending   no source and no image
//	Building  kpack Image not Ready (a previous image may keep running)
//	Deploying workloads rolling out
//	Running   every process has its desired replicas ready
//	Failed    build failed or reconcile error
func (r *AppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	app := &shpyrdv1.App{}
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !app.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := app.DeepCopy()

	out, err := r.reconcile(ctx, app)
	if err != nil {
		logger.Error(err, "reconcile failed")
		app.Status.Phase = shpyrdv1.PhaseFailed
		app.Status.Message = err.Error()
		setCondition(app, shpyrdv1.ConditionReady, metav1.ConditionFalse, "ReconcileError", err.Error())
		r.Recorder.Event(app, corev1.EventTypeWarning, "ReconcileError", err.Error())
	}
	app.Status.ObservedGeneration = app.Generation

	// Optimistic locking: a reconcile working from a stale cache read must
	// not overwrite a newer status (e.g. re-record a release). Conflicts
	// simply requeue. Nothing else may write the App before this patch, or
	// the resourceVersion check fails.
	if statusErr := r.Status().Patch(ctx, app, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); statusErr != nil {
		if apierrors.IsConflict(statusErr) {
			logger.V(1).Info("status conflict, requeueing")
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("update status: %w", statusErr)
	}

	// Side effects only once the status is durable.
	if out.newRelease != nil {
		r.Recorder.Eventf(app, corev1.EventTypeNormal, "Release", "v%d: %s", out.newRelease.Number, out.newRelease.Description)
	}
	if out.clearNote {
		r.clearReleaseNote(ctx, app)
	}
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return out.result, nil
}

// outcome carries what Reconcile must do after the status has been written.
type outcome struct {
	result     ctrl.Result
	newRelease *shpyrdv1.Release
	clearNote  bool
}

func requeue(d time.Duration) outcome { return outcome{result: ctrl.Result{RequeueAfter: d}} }

func (r *AppReconciler) reconcile(ctx context.Context, app *shpyrdv1.App) (outcome, error) {
	// 0. A requested rollback restores that release's config vars first, so
	// the release recorded below carries both the build and the config.
	var secret *corev1.Secret
	if n, err := strconv.Atoi(app.Annotations[shpyrdv1.AnnotationRollbackTo]); err == nil && n > 0 {
		restored, err := r.restoreRelease(ctx, app, n)
		if err != nil {
			return outcome{}, err
		}
		secret = restored
	}

	// 1. Configuration fingerprint.
	if secret == nil {
		secret = &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.EnvSecretName()}, secret); err != nil {
			if !apierrors.IsNotFound(err) {
				return outcome{}, fmt.Errorf("get env secret: %w", err)
			}
			secret = nil
		}
	}
	sizeByProcess := r.processSizes(ctx, app)
	hash := configHash(app, secret, sizeByProcess)

	// 2. Image: pinned, or produced by kpack from the source.
	image := app.Spec.Image
	var build buildState
	var kpackBuild *unstructured.Unstructured
	if app.HasSource() {
		img, err := r.reconcileKpackImage(ctx, app)
		if err != nil {
			return outcome{}, err
		}
		build = readBuildState(img)
		app.Status.LatestBuild = build.LatestBuild
		if image == "" {
			image = build.LatestImage
		}
		if build.LatestBuild != "" {
			kpackBuild = r.getBuild(ctx, app.Namespace, build.LatestBuild)
		}
		switch build.Ready {
		case "True":
			setCondition(app, shpyrdv1.ConditionBuilt, metav1.ConditionTrue, "BuildSucceeded", "image "+shortImage(build.LatestImage))
		case "False":
			setCondition(app, shpyrdv1.ConditionBuilt, metav1.ConditionFalse, "BuildFailed", build.Message)
		default:
			setCondition(app, shpyrdv1.ConditionBuilt, metav1.ConditionUnknown, "Building", firstNonEmpty(build.Message, "build in progress"))
		}
	} else {
		app.Status.LatestBuild = ""
		if image != "" {
			setCondition(app, shpyrdv1.ConditionBuilt, metav1.ConditionTrue, "PrebuiltImage", "using "+shortImage(image))
		}
	}

	// 3. Nothing to run yet.
	if image == "" {
		if !app.HasSource() {
			app.Status.Phase = shpyrdv1.PhasePending
			app.Status.Message = "no source or image; run `shpyrd deploy`"
			setCondition(app, shpyrdv1.ConditionReady, metav1.ConditionFalse, "Pending", app.Status.Message)
			return outcome{}, nil
		}
		if build.Ready == "False" {
			app.Status.Phase = shpyrdv1.PhaseFailed
			app.Status.Message = "build failed: " + build.Message
			setCondition(app, shpyrdv1.ConditionReady, metav1.ConditionFalse, "BuildFailed", build.Message)
			return outcome{}, nil
		}
		app.Status.Phase = shpyrdv1.PhaseBuilding
		app.Status.Message = firstNonEmpty(build.Message, "waiting for the first build")
		setCondition(app, shpyrdv1.ConditionReady, metav1.ConditionFalse, "Building", app.Status.Message)
		return requeue(30 * time.Second), nil
	}

	// 4. Workloads.
	procStatus, err := r.reconcileWorkloads(ctx, app, image, hash)
	if err != nil {
		return outcome{}, err
	}
	app.Status.Image = image
	app.Status.Processes = procStatus
	if hasWeb(app) {
		app.Status.URL = r.Config.url(app)
	} else {
		app.Status.URL = ""
	}

	// 5. Release history and phase.
	var out outcome
	configDesc := ""
	if cur := app.CurrentRelease(); cur != nil && cur.Image == image && cur.ConfigHash != hash {
		configDesc = r.describeConfigChangeSince(ctx, app, cur.Number, secret)
	}
	if recordRelease(app, image, hash, sourceID(app, kpackBuild), metav1.Now(), configDesc, sizeByProcess) {
		out.newRelease = app.CurrentRelease()
		for _, p := range processes(app) {
			out.newRelease.Processes = append(out.newRelease.Processes, p.Name)
		}
		if err := r.snapshotRelease(ctx, app, out.newRelease.Number, secret); err != nil {
			return outcome{}, err
		}
		r.pruneSnapshots(ctx, app)
	}
	// The note and rollback request are one-shot: consume them whether or
	// not they produced a release, so they cannot label an unrelated later
	// change. While a build for the noted change is still running they must
	// survive until the image lands.
	if (app.Annotations[shpyrdv1.AnnotationReleaseNote] != "" || app.Annotations[shpyrdv1.AnnotationRollbackTo] != "") &&
		(!app.HasSource() || app.Spec.Image != "" || build.Ready == "True") {
		out.clearNote = true
	}

	ready, summary := summarizeProcesses(procStatus)
	failing, failMsg := failingSummary(procStatus)
	if cur := app.CurrentRelease(); cur != nil && !ready {
		summary = fmt.Sprintf("Releasing v%d: %s", cur.Number, summary)
	}
	switch {
	case failing && !ready:
		// New instances cannot start; older ones may still be serving.
		app.Status.Phase = shpyrdv1.PhaseFailed
		app.Status.Message = failMsg
		setCondition(app, shpyrdv1.ConditionReady, metav1.ConditionFalse, "InstancesFailing", failMsg)
		out.result = ctrl.Result{RequeueAfter: 30 * time.Second}
		return out, nil
	case app.HasSource() && app.Spec.Image == "" && build.Ready == "False":
		app.Status.Phase = shpyrdv1.PhaseFailed
		app.Status.Message = "build failed (previous release still running): " + build.Message
		setCondition(app, shpyrdv1.ConditionReady, metav1.ConditionFalse, "BuildFailed", build.Message)
	case app.HasSource() && app.Spec.Image == "" && build.Ready != "True":
		app.Status.Phase = shpyrdv1.PhaseBuilding
		app.Status.Message = firstNonEmpty(build.Message, "building new release; "+summary)
		setCondition(app, shpyrdv1.ConditionReady, metav1.ConditionFalse, "Building", app.Status.Message)
		out.result = ctrl.Result{RequeueAfter: 30 * time.Second}
	case !ready:
		app.Status.Phase = shpyrdv1.PhaseDeploying
		app.Status.Message = summary
		setCondition(app, shpyrdv1.ConditionReady, metav1.ConditionFalse, "Deploying", app.Status.Message)
		out.result = ctrl.Result{RequeueAfter: 15 * time.Second}
	default:
		app.Status.Phase = shpyrdv1.PhaseRunning
		app.Status.Message = summary
		setCondition(app, shpyrdv1.ConditionReady, metav1.ConditionTrue, "Running", summary)
	}
	return out, nil
}

// reconcileKpackImage creates or updates the kpack Image and returns its
// current state.
func (r *AppReconciler) reconcileKpackImage(ctx context.Context, app *shpyrdv1.App) (*unstructured.Unstructured, error) {
	desired := r.Config.desiredKpackImage(app)
	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return nil, err
	}

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(KpackImageGVK)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), current)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("create kpack image: %w", err)
		}
		r.Recorder.Event(app, corev1.EventTypeNormal, "BuildRequested", "created kpack Image "+desired.GetName())
		return desired, nil
	case err != nil:
		return nil, fmt.Errorf("get kpack image: %w", err)
	}

	// Compare the fields we own; spec.tag is immutable and unchanged.
	if !equalJSON(current.Object["spec"], desired.Object["spec"]) || !labelsSubset(current.GetLabels(), desired.GetLabels()) {
		updated := current.DeepCopy()
		updated.Object["spec"] = desired.Object["spec"]
		updated.SetLabels(mergeMaps(updated.GetLabels(), desired.GetLabels()))
		updated.SetOwnerReferences(desired.GetOwnerReferences())
		if err := r.Update(ctx, updated); err != nil {
			return nil, fmt.Errorf("update kpack image: %w", err)
		}
		r.Recorder.Event(app, corev1.EventTypeNormal, "BuildRequested", "updated kpack Image source")
		return updated, nil
	}
	return current, nil
}

// processSizes resolves the size name of every process against the catalog
// (for the release fingerprint); unresolvable sizes are recorded as given.
func (r *AppReconciler) processSizes(ctx context.Context, app *shpyrdv1.App) map[string]string {
	catalog := r.catalog(ctx)
	out := map[string]string{}
	for _, p := range processes(app) {
		if _, name, err := processResources(p, catalog); err == nil {
			out[p.Name] = name
		} else {
			out[p.Name] = firstNonEmpty(p.Size, "custom")
		}
	}
	return out
}

// getBuild reads a kpack Build without populating the cache; failures only
// degrade the release description.
func (r *AppReconciler) getBuild(ctx context.Context, namespace, name string) *unstructured.Unstructured {
	b := &unstructured.Unstructured{}
	b.SetGroupVersionKind(KpackBuildGVK)
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, b); err != nil {
		return nil
	}
	return b
}

// reconcileWorkloads makes Deployments, Services and the Ingress match the
// process map and removes workloads of dropped process types.
func (r *AppReconciler) reconcileWorkloads(ctx context.Context, app *shpyrdv1.App, image, hash string) (map[string]shpyrdv1.ProcessStatus, error) {
	status := map[string]shpyrdv1.ProcessStatus{}
	wanted := map[string]bool{}

	catalog := r.catalog(ctx)
	for _, p := range processes(app) {
		wanted[p.Name] = true
		res, sizeName, err := processResources(p, catalog)
		if err != nil {
			return nil, err
		}
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: workloadName(app, p.Name), Namespace: app.Namespace}}
		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, d, func() error {
			r.Config.mutateDeployment(app, p, image, hash, res, d)
			return controllerutil.SetControllerReference(app, d, r.Scheme)
		})
		if err != nil {
			return nil, fmt.Errorf("deployment %s: %w", d.Name, err)
		}
		if op != controllerutil.OperationResultNone {
			log.FromContext(ctx).Info("deployment reconciled", "name", d.Name, "op", op)
		}
		ps := shpyrdv1.ProcessStatus{
			Desired: p.replicas(),
			Ready:   d.Status.ReadyReplicas,
			Updated: d.Status.UpdatedReplicas,
		}
		// The Deployment controller needs a moment after an update.
		if d.Status.ObservedGeneration < d.Generation {
			ps.Ready, ps.Updated = 0, 0
		}
		ps.Failing, ps.Reason = r.processHealth(ctx, app, p.Name)
		ps.Size = sizeName
		ps.CPU = res.Requests.Cpu().String()
		ps.Memory = res.Requests.Memory().String()
		status[p.Name] = ps

		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: workloadName(app, p.Name), Namespace: app.Namespace}}
		if p.port() > 0 {
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
				r.Config.mutateService(app, p, svc)
				return controllerutil.SetControllerReference(app, svc, r.Scheme)
			}); err != nil {
				return nil, fmt.Errorf("service %s: %w", svc.Name, err)
			}
		} else if err := r.deleteIfExists(ctx, svc); err != nil {
			return nil, err
		}
	}

	// Ingress for web.
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}}
	if hasWeb(app) {
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
			r.Config.mutateIngress(app, ing)
			return controllerutil.SetControllerReference(app, ing, r.Scheme)
		}); err != nil {
			return nil, fmt.Errorf("ingress: %w", err)
		}
	} else if err := r.deleteIfExists(ctx, ing); err != nil {
		return nil, err
	}

	// Garbage collect workloads of removed process types.
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(app.Namespace), client.MatchingLabels{shpyrdv1.LabelApp: app.Name}); err != nil {
		return nil, err
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		proc := d.Labels[shpyrdv1.LabelProcess]
		if wanted[proc] || !metav1.IsControlledBy(d, app) {
			continue
		}
		if err := r.deleteIfExists(ctx, d); err != nil {
			return nil, err
		}
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace}}
		if err := r.deleteIfExists(ctx, svc); err != nil {
			return nil, err
		}
	}
	return status, nil
}

func (r *AppReconciler) deleteIfExists(ctx context.Context, obj client.Object) error {
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %T %s: %w", obj, obj.GetName(), err)
	}
	return nil
}

// clearReleaseNote removes the one-shot annotations once consumed.
func (r *AppReconciler) clearReleaseNote(ctx context.Context, app *shpyrdv1.App) {
	patch := client.MergeFrom(app.DeepCopy())
	delete(app.Annotations, shpyrdv1.AnnotationReleaseNote)
	delete(app.Annotations, shpyrdv1.AnnotationRollbackTo)
	if err := r.Patch(ctx, app, patch); err != nil {
		log.FromContext(ctx).Info("could not clear release note", "err", err.Error())
	}
}

func hasWeb(app *shpyrdv1.App) bool {
	for _, p := range processes(app) {
		if p.Name == "web" && p.port() > 0 {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func labelsSubset(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

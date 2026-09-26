package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/ext"
)

// ResourceView is any resource of a project in the shared shape the
// dashboard renders (RFC-0003): apps, volumes and, later, databases.
type ResourceView struct {
	Kind    string `json:"kind"` // App, Volume, ...
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`
	// Endpoint is the URL or host:port to reach the resource, when any.
	Endpoint string `json:"endpoint,omitempty"`
	// Details are kind-specific facts for display (size, mode, release).
	Details map[string]string `json:"details,omitempty"`
	// AttachedTo lists the apps (or app/process) using the resource.
	AttachedTo []string `json:"attachedTo"`
	// Data marks resources whose deletion loses data.
	Data      bool      `json:"data"`
	CreatedAt time.Time `json:"createdAt"`
	// Bindable resources can be attached to apps (Postgres, Redis).
	Bindable bool `json:"bindable"`
}

// listProjectResources returns every resource of the project namespace.
func (s *Server) listProjectResources(c *gin.Context) {
	ns := s.projectNamespace(c)
	ctx := c.Request.Context()
	var apps shpyrdv1.AppList
	if err := s.apps.List(ctx, &apps, client.InNamespace(ns)); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	var vols shpyrdv1.VolumeList
	if err := s.apps.List(ctx, &vols, client.InNamespace(ns)); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]ResourceView, 0, len(apps.Items)+len(vols.Items))
	// Who attaches what, for the attachedTo column of bindable resources.
	attached := map[string][]string{} // "Kind/name" -> apps
	for _, a := range apps.Items {
		for _, b := range a.Spec.Bindings {
			attached[b.Kind+"/"+b.Name] = append(attached[b.Kind+"/"+b.Name], a.Name)
		}
	}
	for _, a := range apps.Items {
		v := ResourceView{
			Kind: "App", Name: a.Name, Phase: firstNonEmpty(a.Status.Phase, shpyrdv1.PhasePending), Message: a.Status.Message,
			Endpoint: a.Status.URL, Details: map[string]string{}, AttachedTo: []string{}, CreatedAt: a.CreationTimestamp.Time,
		}
		if cur := a.CurrentRelease(); cur != nil {
			v.Details["release"] = fmt.Sprintf("v%d", cur.Number)
		}
		if a.HasSource() {
			v.Details["build"] = a.BuildStrategy()
		}
		for _, b := range a.Spec.Bindings {
			v.Details["bindings"] = appendCSV(v.Details["bindings"], b.Kind+"/"+b.Name)
		}
		out = append(out, v)
	}
	for _, vol := range vols.Items {
		view := volumeView(vol)
		mode := "single-instance"
		if view.Shared {
			mode = "shared"
		}
		v := ResourceView{
			Kind: "Volume", Name: vol.Name, Phase: view.Phase, Message: view.Message,
			Details:    map[string]string{"size": view.Size, "mode": mode},
			AttachedTo: view.MountedBy, Data: true, CreatedAt: vol.CreationTimestamp.Time,
		}
		if view.Capacity != "" && view.Capacity != view.Size {
			v.Details["capacity"] = view.Capacity
		}
		out = append(out, v)
	}
	// Resources of enabled extensions, read generically through their
	// shared status shape.
	for _, t := range s.resourceTypes() {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: t.Group, Version: t.Version, Kind: t.Kind + "List"})
		if err := s.apps.List(ctx, list, client.InNamespace(ns)); err != nil {
			continue // CRD missing: nothing of this kind
		}
		for _, u := range list.Items {
			v := resourceViewOf(t, u)
			v.AttachedTo = attached[t.Kind+"/"+u.GetName()]
			if v.AttachedTo == nil {
				v.AttachedTo = []string{}
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "App" || (out[j].Kind != "App" && out[i].Kind < out[j].Kind)
		}
		return out[i].Name < out[j].Name
	})
	c.JSON(http.StatusOK, out)
}

// resourceTypes lists the resource kinds of the enabled extensions.
func (s *Server) resourceTypes() []ext.ResourceType {
	var out []ext.ResourceType
	for _, x := range s.opts.Extensions {
		out = append(out, x.Types()...)
	}
	return out
}

// resourceType finds an enabled extension kind.
func (s *Server) resourceType(kind string) (ext.ResourceType, bool) {
	for _, t := range s.resourceTypes() {
		if strings.EqualFold(t.Kind, kind) {
			return t, true
		}
	}
	return ext.ResourceType{}, false
}

// resourceViewOf renders any extension resource from the shared status shape.
func resourceViewOf(t ext.ResourceType, u unstructured.Unstructured) ResourceView {
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	msg, _, _ := unstructured.NestedString(u.Object, "status", "message")
	endpoint, _, _ := unstructured.NestedString(u.Object, "status", "endpoint")
	v := ResourceView{
		Kind: t.Kind, Name: u.GetName(), Phase: firstNonEmpty(phase, shpyrdv1.ResourcePending), Message: msg, Endpoint: endpoint,
		Details: map[string]string{}, AttachedTo: []string{}, Data: true, Bindable: t.Bindable, CreatedAt: u.GetCreationTimestamp().Time,
	}
	if spec, ok, _ := unstructured.NestedMap(u.Object, "spec"); ok {
		for k, val := range spec {
			switch x := val.(type) {
			case string:
				v.Details[k] = x
			case bool:
				v.Details[k] = fmt.Sprint(x)
			case int64, float64:
				v.Details[k] = fmt.Sprint(x)
			}
		}
	}
	// Backups (RFC-0038): on/off with retention, the last one and the window.
	if b, ok, _ := unstructured.NestedMap(u.Object, "spec", "backups"); ok && b != nil {
		retention, _ := b["retention"].(string)
		v.Details["backups"] = "daily, kept " + firstNonEmpty(retention, "14d")
		if last, _, _ := unstructured.NestedString(u.Object, "status", "lastBackup"); last != "" {
			v.Details["lastBackup"] = last
		}
		if from, _, _ := unstructured.NestedString(u.Object, "status", "recoverableFrom"); from != "" {
			v.Details["recoverableFrom"] = from
		}
	}
	if rec, ok, _ := unstructured.NestedMap(u.Object, "spec", "recovery"); ok && rec != nil {
		from, _ := rec["from"].(string)
		v.Details["restoredFrom"] = from
	}
	// The disk is the provider's minimum when the request was below it
	// (RFC-0060): show what exists, not what was asked.
	if eff, _, _ := unstructured.NestedString(u.Object, "status", "storage"); eff != "" {
		if req := v.Details["storage"]; req != "" && req != eff {
			v.Details["storage"] = eff + " (" + req + " requested; provider minimum)"
		} else {
			v.Details["storage"] = eff
		}
	}
	return v
}

// CreateResourceRequest creates a resource of an extension kind.
type CreateResourceRequest struct {
	Kind string                 `json:"kind" binding:"required"`
	Name string                 `json:"name" binding:"required"`
	Spec map[string]interface{} `json:"spec"`
}

// createResource creates an extension resource in the project; the CRD
// schema validates the spec.
func (s *Server) createResource(c *gin.Context) {
	var req CreateResourceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	t, ok := s.resourceType(req.Kind)
	if !ok {
		abort(c, http.StatusBadRequest, fmt.Errorf("resources of kind %q are not available on this cluster (enable the extension that provides them)", req.Kind))
		return
	}
	if !volumeName.MatchString(req.Name) {
		abort(c, http.StatusBadRequest, errors.New("name must be lowercase letters, digits and dashes (max 40 chars)"))
		return
	}
	ns := s.projectNamespace(c)
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": t.Group + "/" + t.Version,
		"kind":       t.Kind,
		"metadata": map[string]interface{}{
			"name": req.Name, "namespace": ns,
			"labels": map[string]interface{}{shpyrdv1.LabelManagedBy: "shpyrd", shpyrdv1.LabelProject: c.Param("slug")},
		},
		"spec": req.Spec,
	}}
	if u.Object["spec"] == nil {
		u.Object["spec"] = map[string]interface{}{}
	}
	if err := s.apps.Create(c.Request.Context(), u); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			abort(c, http.StatusConflict, fmt.Errorf("%s %q already exists", t.Kind, req.Name))
		case apierrors.IsInvalid(err):
			abort(c, http.StatusBadRequest, err)
		default:
			abort(c, http.StatusBadGateway, err)
		}
		return
	}
	s.audit(c, c.Param("slug"), "resource.create", t.Kind+" "+req.Name, "")
	c.JSON(http.StatusCreated, resourceViewOf(t, *u))
}

// deleteResource removes an extension resource; attached ones need ?force=true.
func (s *Server) deleteResource(c *gin.Context) {
	t, ok := s.resourceType(c.Param("kind"))
	if !ok {
		abort(c, http.StatusNotFound, errors.New("unknown resource kind"))
		return
	}
	ns, name := s.projectNamespace(c), c.Param("name")
	var apps shpyrdv1.AppList
	_ = s.apps.List(c.Request.Context(), &apps, client.InNamespace(ns))
	var bound []string
	for _, a := range apps.Items {
		for _, b := range a.Spec.Bindings {
			if strings.EqualFold(b.Kind, t.Kind) && b.Name == name {
				bound = append(bound, a.Name)
			}
		}
	}
	if len(bound) > 0 && c.Query("force") != "true" {
		abort(c, http.StatusConflict, fmt.Errorf("%s %s is attached to %s: detach it first (or force)", t.Kind, name, strings.Join(bound, ", ")))
		return
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: t.Group, Version: t.Version, Kind: t.Kind})
	u.SetNamespace(ns)
	u.SetName(name)
	if err := s.apps.Delete(c.Request.Context(), u); err != nil {
		abortNotFound(c, err, strings.ToLower(t.Kind))
		return
	}
	s.audit(c, c.Param("slug"), "resource.delete", t.Kind+" "+name, "")
	c.Status(http.StatusNoContent)
}

// ---- bindings (attach / detach) -----------------------------------------------

// BindingRequest attaches a resource to the app.
type BindingRequest struct {
	Kind   string `json:"kind" binding:"required"`
	Name   string `json:"name" binding:"required"`
	Prefix string `json:"prefix,omitempty"`
}

var prefixRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,30}$`)

// attachResource adds a binding: a config release once the resource is ready.
func (s *Server) attachResource(c *gin.Context) {
	var req BindingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	t, ok := s.resourceType(req.Kind)
	if !ok || !t.Bindable {
		abort(c, http.StatusBadRequest, fmt.Errorf("resources of kind %q cannot be attached", req.Kind))
		return
	}
	if req.Prefix != "" && !prefixRe.MatchString(req.Prefix) {
		abort(c, http.StatusBadRequest, errors.New("prefix must be letters, digits and underscores"))
		return
	}
	ns := s.projectNamespace(c)
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: t.Group, Version: t.Version, Kind: t.Kind})
	if err := s.apps.Get(c.Request.Context(), types.NamespacedName{Namespace: ns, Name: req.Name}, u); err != nil {
		abortNotFound(c, err, strings.ToLower(t.Kind)+" "+req.Name)
		return
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		for _, b := range a.Spec.Bindings {
			if b.Kind == t.Kind && b.Name == req.Name {
				return fmt.Errorf("%s %s is already attached", t.Kind, req.Name)
			}
		}
		a.Spec.Bindings = append(a.Spec.Bindings, shpyrdv1.Binding{Kind: t.Kind, Name: req.Name, Prefix: strings.ToUpper(req.Prefix)})
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		a.Annotations[shpyrdv1.AnnotationReleaseNote] = fmt.Sprintf("Attach %s %s", t.Kind, req.Name)
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "attach", t.Kind+" "+req.Name, req.Prefix)
	c.JSON(http.StatusOK, summarize(app))
}

// detachResource removes a binding.
func (s *Server) detachResource(c *gin.Context) {
	kind, name := c.Param("kind"), c.Param("name")
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		kept := a.Spec.Bindings[:0]
		found := false
		for _, b := range a.Spec.Bindings {
			if strings.EqualFold(b.Kind, kind) && b.Name == name {
				found = true
				continue
			}
			kept = append(kept, b)
		}
		if !found {
			return fmt.Errorf("%s %s is not attached", kind, name)
		}
		a.Spec.Bindings = kept
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		a.Annotations[shpyrdv1.AnnotationReleaseNote] = fmt.Sprintf("Detach %s %s", kind, name)
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "detach", kind+" "+name, "")
	c.JSON(http.StatusOK, summarize(app))
}

func appendCSV(list, item string) string {
	if list == "" {
		return item
	}
	return list + ", " + item
}

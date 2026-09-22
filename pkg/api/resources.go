package api

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
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
}

// listProjectResources returns every resource of the project namespace.
func (s *Server) listProjectResources(c *gin.Context) {
	ns := c.Param("ns")
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "App" || (out[j].Kind != "App" && out[i].Kind < out[j].Kind)
		}
		return out[i].Name < out[j].Name
	})
	c.JSON(http.StatusOK, out)
}

func appendCSV(list, item string) string {
	if list == "" {
		return item
	}
	return list + ", " + item
}

package api

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/sizes"
	"github.com/shpyrd-io/shpyrd/pkg/store"
)

// Plan limits (RFC-0033 phase 6, RFC-0042). A workspace's plan is a set of
// ceilings on what its projects may use together: projects, instances,
// CPU and memory requests, storage. The API checks them before it changes
// anything, so people read "this would use 5 instances of the plan's 4"
// rather than finding pods stuck Pending; the controller backs the check
// with a ResourceQuota per project namespace. The open-source platform
// sets no plan; the cloud layer does, per workspace.

// Usage is what a workspace uses, in the plan's terms.
type Usage struct {
	Projects  int    `json:"projects"`
	Instances int    `json:"instances"`
	CPU       string `json:"cpu"`
	Memory    string `json:"memory"`
	Storage   string `json:"storage"`
}

type usage struct {
	projects, instances  int
	cpu, memory, storage resource.Quantity
}

func (u usage) view() Usage {
	return Usage{Projects: u.projects, Instances: u.instances, CPU: u.cpu.String(), Memory: u.memory.String(), Storage: u.storage.String()}
}

// addApp adds an App's desired instances and their requests. A project
// with nothing to run yet (no image, no source) is a project and nothing
// more.
func (u *usage) addApp(app *shpyrdv1.App, cat *sizes.Catalog) {
	u.projects++
	if app.Spec.Image == "" && !app.HasSource() {
		return
	}
	procs := app.Spec.Processes
	if len(procs) == 0 {
		procs = map[string]shpyrdv1.Process{"web": {}}
	}
	for _, p := range procs {
		n := int32(1)
		if p.Replicas != nil {
			n = *p.Replicas
		}
		if n <= 0 {
			continue
		}
		u.instances += int(n)
		res, _, err := cat.Resolve(p.Size, p.Resources)
		if err != nil {
			continue // an unknown size is refused elsewhere
		}
		if cpu, ok := res.Requests[corev1.ResourceCPU]; ok {
			for i := int32(0); i < n; i++ {
				u.cpu.Add(cpu)
			}
		}
		if mem, ok := res.Requests[corev1.ResourceMemory]; ok {
			for i := int32(0); i < n; i++ {
				u.memory.Add(mem)
			}
		}
	}
}

// planOf returns the workspace's limits, nil when it has none.
func (s *Server) planOf(ctx context.Context, ws string) *store.Limits {
	w, err := s.store.Workspace(ctx, ws)
	if err != nil || w.Settings.Limits == nil {
		return nil
	}
	return w.Settings.Limits
}

// workspaceUsage sums a workspace's projects. When replace is not nil it
// stands in for the stored App of the same name (the state after a
// change); when add is not nil it is counted as a new project.
func (s *Server) workspaceUsage(ctx context.Context, ws string, cat *sizes.Catalog, replace *shpyrdv1.App) (usage, error) {
	var u usage
	var apps shpyrdv1.AppList
	if err := s.apps.List(ctx, &apps); err != nil {
		return u, err
	}
	seen := false
	for i := range apps.Items {
		a := &apps.Items[i]
		if workspaceOf(a) != ws {
			continue
		}
		if replace != nil && a.Namespace == replace.Namespace && a.Name == replace.Name {
			u.addApp(replace, cat)
			seen = true
			continue
		}
		u.addApp(a, cat)
	}
	if replace != nil && !seen {
		u.addApp(replace, cat)
	}
	var vols shpyrdv1.VolumeList
	if err := s.apps.List(ctx, &vols); err == nil {
		for i := range vols.Items {
			v := &vols.Items[i]
			if volumeWorkspace(v) != ws {
				continue
			}
			u.storage.Add(v.Spec.Size)
		}
	}
	return u, nil
}

// volumeWorkspace is the workspace a Volume belongs to: its label
// (authoritative, set at creation), else the implicit workspace, the only
// one that existed before labels.
func volumeWorkspace(v *shpyrdv1.Volume) string {
	if ws := v.Labels[shpyrdv1.LabelWorkspace]; ws != "" {
		return ws
	}
	return store.DefaultWorkspace
}

// Axes of a plan a check may look at: a storage change is judged on
// storage, a scale on instances and requests, so a workspace already over
// on one axis (a plan that shrank) can still act on the others.
type axis int

const (
	axisProjects axis = iota
	axisCompute       // instances, CPU, memory
	axisStorage
)

// exceeds explains the first ceiling a usage breaks on the given axes, or "".
func exceeds(l *store.Limits, u usage, axes ...axis) string {
	if l == nil {
		return ""
	}
	for _, a := range axes {
		switch a {
		case axisProjects:
			if l.Projects > 0 && u.projects > l.Projects {
				return fmt.Sprintf("this would make %d projects; the plan allows %d", u.projects, l.Projects)
			}
		case axisCompute:
			if l.Instances > 0 && u.instances > l.Instances {
				return fmt.Sprintf("this would run %d instances; the plan allows %d", u.instances, l.Instances)
			}
			if q, err := resource.ParseQuantity(l.CPU); err == nil && l.CPU != "" && u.cpu.Cmp(q) > 0 {
				return fmt.Sprintf("this would request %s CPU; the plan allows %s", u.cpu.String(), q.String())
			}
			if q, err := resource.ParseQuantity(l.Memory); err == nil && l.Memory != "" && u.memory.Cmp(q) > 0 {
				return fmt.Sprintf("this would request %s of memory; the plan allows %s", u.memory.String(), q.String())
			}
		case axisStorage:
			if q, err := resource.ParseQuantity(l.Storage); err == nil && l.Storage != "" && u.storage.Cmp(q) > 0 {
				return fmt.Sprintf("this would claim %s of storage; the plan allows %s", u.storage.String(), q.String())
			}
		}
	}
	return ""
}

// checkPlan refuses a change to an App that would break the workspace's
// plan; after is the App as it would be stored.
func (s *Server) checkPlan(ctx context.Context, ws string, after *shpyrdv1.App) error {
	limits := s.planOf(ctx, ws)
	if limits == nil {
		return nil
	}
	cat, err := s.catalog(ctx)
	if err != nil {
		return err
	}
	u, err := s.workspaceUsage(ctx, ws, cat, after)
	if err != nil {
		return err
	}
	if msg := exceeds(limits, u, axisCompute); msg != "" {
		return fmt.Errorf("plan limit: %s", msg)
	}
	return nil
}

// checkPlanStorage refuses claiming more storage than the plan allows;
// extra is the size being added (or the growth of a resize).
func (s *Server) checkPlanStorage(ctx context.Context, ws string, extra resource.Quantity) error {
	limits := s.planOf(ctx, ws)
	if limits == nil || limits.Storage == "" {
		return nil
	}
	cat, err := s.catalog(ctx)
	if err != nil {
		return err
	}
	u, err := s.workspaceUsage(ctx, ws, cat, nil)
	if err != nil {
		return err
	}
	u.storage.Add(extra)
	if msg := exceeds(limits, u, axisStorage); msg != "" {
		return fmt.Errorf("plan limit: %s", msg)
	}
	return nil
}

// usageOf reports a workspace's current usage for the dashboard.
func (s *Server) usageOf(ctx context.Context, ws string) *Usage {
	cat, err := s.catalog(ctx)
	if err != nil {
		return nil
	}
	u, err := s.workspaceUsage(ctx, ws, cat, nil)
	if err != nil {
		return nil
	}
	v := u.view()
	return &v
}

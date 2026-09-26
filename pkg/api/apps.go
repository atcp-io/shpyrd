package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/project"
)

// AppSummary is the list view of a project. Image references are reduced
// to their digest so registry internals never surface in the UI.
type AppSummary struct {
	// Slug identifies the project in URLs, the CLI and hostnames.
	Slug string `json:"slug"`
	// DisplayName is the human name; the slug when none was given.
	DisplayName string                            `json:"displayName"`
	Namespace   string                            `json:"namespace"`
	Phase       string                            `json:"phase"`
	Message     string                            `json:"message,omitempty"`
	URL         string                            `json:"url,omitempty"`
	Digest      string                            `json:"digest,omitempty"`
	Release     int                               `json:"release"`
	Source      string                            `json:"source,omitempty"`
	Processes   map[string]shpyrdv1.ProcessStatus `json:"processes,omitempty"`
	CreatedAt   time.Time                         `json:"createdAt"`
	// Exposure is "external" (public LB, default) or "internal" (private LB,
	// RFC-0036). Empty means external.
	Exposure string `json:"exposure,omitempty"`
	// Access is who may open the app: public, authenticated or identified
	// (RFC-0033). Apps created before the field exist are public.
	Access string `json:"access"`
}

func summarize(a *shpyrdv1.App) AppSummary {
	s := AppSummary{
		Exposure:    a.Spec.Exposure,
		Access:      a.EffectiveAccess(),
		Slug:        a.Name,
		DisplayName: project.DisplayName(a),
		Namespace:   a.Namespace,
		Phase:       a.Status.Phase,
		Message:     a.Status.Message,
		URL:         a.Status.URL,
		Digest:      Digest(a.Status.Image),
		Processes:   a.Status.Processes,
		CreatedAt:   a.CreationTimestamp.Time,
	}
	if s.Phase == "" {
		s.Phase = shpyrdv1.PhasePending
	}
	if r := a.CurrentRelease(); r != nil {
		s.Release = r.Number
	}
	if a.Spec.Source != nil {
		switch {
		case a.Spec.Source.Git != nil:
			s.Source = a.Spec.Source.Git.URL
		case a.Spec.Source.Blob != nil:
			s.Source = "archive"
		}
	}
	return s
}

// Digest returns the sha256 part of an image reference ("sha256:abcd..."
// shortened to 12 hex characters), or "" when the reference has none.
func Digest(image string) string {
	if i := strings.Index(image, "@sha256:"); i >= 0 {
		d := image[i+8:]
		if len(d) > 12 {
			d = d[:12]
		}
		return d
	}
	return ""
}

// AppDetail is the App with image references replaced by digests.
type AppDetail struct {
	Slug        string                            `json:"slug"`
	DisplayName string                            `json:"displayName"`
	Namespace   string                            `json:"namespace"`
	CreatedAt   time.Time                         `json:"createdAt"`
	Spec        AppDetailSpec                     `json:"spec"`
	Status      AppDetailStatus                   `json:"status"`
	Processes   map[string]shpyrdv1.ProcessStatus `json:"processes,omitempty"`
}

type AppDetailSpec struct {
	Source      *shpyrdv1.Source            `json:"source,omitempty"`
	PinnedImage string                      `json:"pinnedDigest,omitempty"`
	Processes   map[string]shpyrdv1.Process `json:"processes,omitempty"`
	Env         []corev1.EnvVar             `json:"env,omitempty"`
	Domains     []string                    `json:"domains,omitempty"`
	Build       *shpyrdv1.Build             `json:"build,omitempty"`
	Bindings    []shpyrdv1.Binding          `json:"bindings,omitempty"`
	Exposure    string                      `json:"exposure,omitempty"`
	Access      string                      `json:"access"` // who may open the app (RFC-0033)
}

type AppDetailStatus struct {
	Phase       string             `json:"phase"`
	Message     string             `json:"message,omitempty"`
	Digest      string             `json:"digest,omitempty"`
	URL         string             `json:"url,omitempty"`
	LatestBuild string             `json:"latestBuild,omitempty"`
	Releases    []ReleaseView      `json:"releases"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
	// Domains is the state of each custom domain (RFC-0034).
	Domains []shpyrdv1.DomainStatus `json:"domains,omitempty"`
}

// ReleaseView is a release with the image reduced to its digest and linked
// to the build that produced it (when kpack still has that Build).
type ReleaseView struct {
	Number      int       `json:"number"`
	Digest      string    `json:"digest"`
	Build       int       `json:"build,omitempty"`
	Source      string    `json:"source,omitempty"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	Processes   []string  `json:"processes,omitempty"`
	// Kind classifies the release: deploy, config or rollback.
	Kind string `json:"kind"`
}

// releaseKind classifies a release from what changed: a rollback by its
// note, a deploy when the build changed, otherwise a config release
// (config vars, sizes, attachments, globals).
func releaseKind(prev *shpyrdv1.Release, cur shpyrdv1.Release) string {
	switch {
	case strings.HasPrefix(cur.Description, "Rollback"):
		return "rollback"
	case prev == nil || prev.Image != cur.Image:
		return "deploy"
	default:
		return "config"
	}
}

func detail(a *shpyrdv1.App, buildByDigest map[string]int) AppDetail {
	d := AppDetail{
		Slug:        a.Name,
		DisplayName: project.DisplayName(a),
		Namespace:   a.Namespace,
		CreatedAt:   a.CreationTimestamp.Time,
		Spec: AppDetailSpec{
			Source:      a.Spec.Source,
			PinnedImage: Digest(a.Spec.Image),
			Processes:   a.Spec.Processes,
			Env:         a.Spec.Env,
			Domains:     a.Spec.Domains,
			Build:       a.Spec.Build,
			Bindings:    a.Spec.Bindings,
			Exposure:    a.Spec.Exposure,
			Access:      a.EffectiveAccess(),
		},
		Status: AppDetailStatus{
			Phase:       firstNonEmpty(a.Status.Phase, shpyrdv1.PhasePending),
			Message:     a.Status.Message,
			Digest:      Digest(a.Status.Image),
			URL:         a.Status.URL,
			LatestBuild: a.Status.LatestBuild,
			Releases:    []ReleaseView{},
			Conditions:  a.Status.Conditions,
			Domains:     a.Status.Domains,
		},
		Processes: a.Status.Processes,
	}
	if a.Spec.Source != nil && a.Spec.Source.Blob != nil {
		// The blob URL is an internal address; keep only the identity.
		blob := *a.Spec.Source.Blob
		blob.URL = ""
		src := *a.Spec.Source
		src.Blob = &blob
		d.Spec.Source = &src
	}
	for i, r := range a.Status.Releases {
		dg := Digest(r.Image)
		var prev *shpyrdv1.Release
		if i > 0 {
			prev = &a.Status.Releases[i-1]
		}
		d.Status.Releases = append(d.Status.Releases, ReleaseView{
			Number: r.Number, Digest: dg, Build: buildByDigest[dg], Source: r.Source, Description: r.Description,
			CreatedAt: r.CreatedAt.Time, Processes: r.Processes, Kind: releaseKind(prev, r),
		})
	}
	return d
}

// rolloutInProgress reports whether the last requested change is still being
// rolled out; stacking another release on top only adds confusion.
func rolloutInProgress(a *shpyrdv1.App) bool {
	return a.Status.ObservedGeneration < a.Generation || a.Status.Phase == shpyrdv1.PhaseDeploying || a.Status.Phase == shpyrdv1.PhaseBuilding
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) listApps(c *gin.Context) {
	var list shpyrdv1.AppList
	if err := s.apps.List(c.Request.Context(), &list); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]AppSummary, 0, len(list.Items))
	ws := s.workspace(c)
	for i := range list.Items {
		if workspaceOf(&list.Items[i]) != ws || !s.canView(c, list.Items[i].Name) {
			continue
		}
		out = append(out, summarize(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayName != out[j].DisplayName {
			return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName)
		}
		return out[i].Slug < out[j].Slug
	})
	c.JSON(http.StatusOK, out)
}

// projectKey locates the App of the project named in the path, in the
// request's workspace (RFC-0033: the namespace carries the workspace).
func (s *Server) projectKey(c *gin.Context) types.NamespacedName {
	slug := c.Param("slug")
	return types.NamespacedName{Namespace: s.projectNamespace(c), Name: slug}
}

// projectNamespace is the namespace of the project named in the path.
func (s *Server) projectNamespace(c *gin.Context) string {
	return project.NamespaceIn(s.workspace(c), c.Param("slug"))
}

func (s *Server) loadApp(c *gin.Context) (*shpyrdv1.App, bool) {
	app := &shpyrdv1.App{}
	err := s.apps.Get(c.Request.Context(), s.projectKey(c), app)
	if apierrors.IsNotFound(err) {
		abort(c, http.StatusNotFound, errors.New("project not found"))
		return nil, false
	}
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return nil, false
	}
	return app, true
}

func (s *Server) getApp(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, detail(app, s.buildsByDigest(c.Request.Context(), app)))
}

// CreateAppRequest creates a project: namespace app-<slug> plus the App.
// Name is the display name, any text; Slug overrides the one derived from it.
type CreateAppRequest struct {
	Name      string                      `json:"name" binding:"required"`
	Slug      string                      `json:"slug,omitempty"`
	Domains   []string                    `json:"domains,omitempty"`
	Processes map[string]shpyrdv1.Process `json:"processes,omitempty"`
	Git       *shpyrdv1.GitSource         `json:"git,omitempty"`
	SubPath   string                      `json:"subPath,omitempty"`
	// Access is who may open the app (RFC-0033); new projects start
	// authenticated unless asked to be public.
	Access string `json:"access,omitempty"`
}

func (s *Server) createApp(c *gin.Context) {
	var req CreateAppRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	slug := req.Slug
	if slug == "" {
		var err error
		if slug, err = project.Slug(req.Name); err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
	} else if err := project.ValidateSlug(slug); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if project.Reserved(slug) {
		abort(c, http.StatusBadRequest, fmt.Errorf("%q is reserved for the platform; pick another name", slug))
		return
	}
	ws, err := s.tenant(c)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	// app-<workspace>-<project> must stay a DNS label (63 characters).
	if !ws.Implicit() && len(project.NamespaceIn(ws.Slug, slug)) > 63 {
		abort(c, http.StatusBadRequest, fmt.Errorf("slug %q is too long for this workspace: at most %d characters", slug, 63-len("app-"+ws.Slug+"-")))
		return
	}
	ctx := c.Request.Context()
	if limits := s.planOf(ctx, ws.Slug); limits != nil && limits.Projects > 0 {
		cat, err := s.catalog(ctx)
		if err != nil {
			abort(c, http.StatusBadGateway, err)
			return
		}
		u, err := s.workspaceUsage(ctx, ws.Slug, cat, nil)
		if err != nil {
			abort(c, http.StatusBadGateway, err)
			return
		}
		if u.projects+1 > limits.Projects {
			abort(c, http.StatusBadRequest, fmt.Errorf("plan limit: the plan allows %d projects and the workspace has %d", limits.Projects, u.projects))
			return
		}
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   project.NamespaceIn(ws.Slug, slug),
		Labels: project.NamespaceLabels(ws.Slug, slug),
	}}
	if err := s.apps.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		abort(c, http.StatusBadGateway, fmt.Errorf("create namespace: %w", err))
		return
	}
	access := req.Access
	if access == "" {
		access = shpyrdv1.AccessAuthenticated
	}
	if !validAccess(access) {
		abort(c, http.StatusBadRequest, fmt.Errorf("access must be public, authenticated or identified"))
		return
	}
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: slug, Namespace: ns.Name, Labels: project.NamespaceLabels(ws.Slug, slug)},
		Spec:       shpyrdv1.AppSpec{Domains: req.Domains, Processes: req.Processes, Access: access},
	}
	project.SetDisplayName(app, req.Name)
	if req.Git != nil && req.Git.URL != "" {
		app.Spec.Source = &shpyrdv1.Source{Git: req.Git, SubPath: req.SubPath}
	}
	if err := s.apps.Create(ctx, app); err != nil {
		if apierrors.IsAlreadyExists(err) {
			abort(c, http.StatusConflict, fmt.Errorf("project %q already exists; choose another slug, for example %s-2", slug, slug))
			return
		}
		abort(c, http.StatusBadGateway, err)
		return
	}
	s.audit(c, app.Name, "project.create", project.Label(app), "")
	c.JSON(http.StatusCreated, summarize(app))
}

// UpdateAppRequest changes project metadata; only the display name so far.
type UpdateAppRequest struct {
	Name *string `json:"name,omitempty"`
}

// updateApp renames a project (its display name; the slug never changes).
func (s *Server) updateApp(c *gin.Context) {
	var req UpdateAppRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if req.Name == nil {
		abort(c, http.StatusBadRequest, errors.New("nothing to update: give name"))
		return
	}
	if strings.TrimSpace(*req.Name) == "" {
		abort(c, http.StatusBadRequest, errors.New("name must not be empty"))
		return
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		project.SetDisplayName(a, *req.Name)
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "project.rename", project.Label(app), "")
	c.JSON(http.StatusOK, detail(app, s.buildsByDigest(c.Request.Context(), app)))
}

// deployDetail summarises a deploy request for the audit trail.
func deployDetail(req DeployRequest) string {
	if req.Image != "" {
		return "image " + req.Image
	}
	if req.Git != nil {
		d := req.Git.URL + "@" + firstNonEmpty(req.Git.Revision, "main")
		if req.Strategy != "" {
			d += " (" + req.Strategy + ")"
		}
		return d
	}
	return ""
}

// DeployRequest points the app at a new source (or prebuilt image).
type DeployRequest struct {
	Git     *shpyrdv1.GitSource `json:"git,omitempty"`
	SubPath string              `json:"subPath,omitempty"`
	Image   string              `json:"image,omitempty"`
	// Strategy selects "buildpacks" or "dockerfile" for source deploys;
	// empty keeps the app's current setting.
	Strategy string `json:"strategy,omitempty"`
	// Dockerfile is the path inside the directory (dockerfile strategy).
	Dockerfile string `json:"dockerfile,omitempty"`
}

func (s *Server) deployApp(c *gin.Context) {
	var req DeployRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if (req.Git == nil || req.Git.URL == "") && req.Image == "" {
		abort(c, http.StatusBadRequest, errors.New("provide git.url or image"))
		return
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		if req.Image != "" {
			a.Spec.Image = req.Image
			return nil
		}
		a.Spec.Image = ""
		if req.Git.Revision == "" {
			req.Git.Revision = "main"
		}
		a.Spec.Source = &shpyrdv1.Source{Git: req.Git, SubPath: req.SubPath}
		switch req.Strategy {
		case "":
		case shpyrdv1.StrategyBuildpacks, shpyrdv1.StrategyDockerfile:
			if a.Spec.Build == nil {
				a.Spec.Build = &shpyrdv1.Build{}
			}
			a.Spec.Build.Strategy = req.Strategy
			if req.Strategy == shpyrdv1.StrategyDockerfile {
				a.Spec.Build.Dockerfile = req.Dockerfile
			}
		default:
			return fmt.Errorf("strategy must be buildpacks or dockerfile")
		}
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "deploy", app.Name, deployDetail(req))
	c.JSON(http.StatusAccepted, summarize(app))
}

// deleteApp removes the app namespace, which cascades to everything in it.
func (s *Server) deleteApp(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: app.Namespace}}
	if err := s.apps.Delete(c.Request.Context(), ns); err != nil && !apierrors.IsNotFound(err) {
		abort(c, http.StatusBadGateway, err)
		return
	}
	// Grants on a destroyed project go with it (RFC-0033).
	if err := s.store.DeleteProjectGrants(c.Request.Context(), s.workspace(c), app.Name); err != nil {
		s.log.Warn("could not remove the project's grants", "project", app.Name, "err", err.Error())
	} else {
		s.membershipChanged()
	}
	s.audit(c, app.Name, "project.destroy", app.Name, "")
	c.JSON(http.StatusAccepted, gin.H{"status": "deleting"})
}

type scaleRequest struct {
	Process  string `json:"process" binding:"required"`
	Replicas *int32 `json:"replicas" binding:"required"`
}

func (s *Server) scaleApp(c *gin.Context) {
	var req scaleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if *req.Replicas < 0 || *req.Replicas > 100 {
		abort(c, http.StatusBadRequest, errors.New("replicas must be between 0 and 100"))
		return
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		if a.Spec.Processes == nil {
			a.Spec.Processes = map[string]shpyrdv1.Process{"web": {}}
		}
		p, ok := a.Spec.Processes[req.Process]
		if !ok && req.Process != "web" {
			return fmt.Errorf("unknown process %q", req.Process)
		}
		if err := s.checkScale(c.Request.Context(), a, req.Process, *req.Replicas); err != nil {
			return err
		}
		p.Replicas = ptr.To(*req.Replicas)
		a.Spec.Processes[req.Process] = p
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "scale", app.Name, fmt.Sprintf("%s=%d", req.Process, *req.Replicas))
	c.JSON(http.StatusOK, summarize(app))
}

type rollbackRequest struct {
	Release int `json:"release" binding:"required"`
}

func (s *Server) rollbackApp(c *gin.Context) {
	var req rollbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		target := a.ReleaseByNumber(req.Release)
		if target == nil {
			return fmt.Errorf("release v%d not found", req.Release)
		}
		if cur := a.CurrentRelease(); cur != nil && cur.Number == target.Number {
			return fmt.Errorf("v%d is the current release", req.Release)
		}
		if rolloutInProgress(a) {
			return fmt.Errorf("a release is still rolling out (%s); wait for it to finish", firstNonEmpty(a.Status.Message, a.Status.Phase))
		}
		// Pin the build and sizes; the controller restores the config vars snapshot.
		a.Spec.Image = target.Image
		restoreSizes(a, target)
		a.Spec.Bindings = append([]shpyrdv1.Binding(nil), target.Bindings...)
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		a.Annotations[shpyrdv1.AnnotationReleaseNote] = fmt.Sprintf("Rollback to v%d", req.Release)
		a.Annotations[shpyrdv1.AnnotationRollbackTo] = fmt.Sprint(req.Release)
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "rollback", app.Name, fmt.Sprintf("to v%d", req.Release))
	c.JSON(http.StatusOK, summarize(app))
}

// redeployRequest chooses what a redeploy does; empty lets the server
// decide from the project's state.
type redeployRequest struct {
	// Action is "restart" (new instances of the current release) or
	// "rebuild" (build the same source again).
	Action string `json:"action"`
}

// RedeployResult says what the redeploy did.
type RedeployResult struct {
	Action  string     `json:"action"`
	Message string     `json:"message"`
	App     AppSummary `json:"app"`
}

// redeployApp restarts the current release, or builds the same source again
// when the last build failed (or when asked): the button for "try it again"
// that creates no release.
// exposureRequest changes how a project is exposed (RFC-0036).
type exposureRequest struct {
	Exposure string `json:"exposure" binding:"required"`
}

func (s *Server) setExposure(c *gin.Context) {
	var req exposureRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if req.Exposure != "external" && req.Exposure != "internal" {
		abort(c, http.StatusBadRequest, fmt.Errorf("exposure must be external or internal"))
		return
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		a.Spec.Exposure = req.Exposure
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "exposure", app.Name, req.Exposure)
	c.JSON(http.StatusOK, summarize(app))
}

func validAccess(a string) bool {
	return a == shpyrdv1.AccessPublic || a == shpyrdv1.AccessAuthenticated || a == shpyrdv1.AccessIdentified
}

// accessRequest changes who may open the app (RFC-0033).
type accessRequest struct {
	Access string `json:"access" binding:"required"`
}

func (s *Server) setAccess(c *gin.Context) {
	var req accessRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if !validAccess(req.Access) {
		abort(c, http.StatusBadRequest, fmt.Errorf("access must be public, authenticated or identified"))
		return
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		a.Spec.Access = req.Access
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "access", app.Name, req.Access)
	c.JSON(http.StatusOK, summarize(app))
}

func (s *Server) redeployApp(c *gin.Context) {
	var req redeployRequest
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
	}
	if req.Action != "" && req.Action != "restart" && req.Action != "rebuild" {
		abort(c, http.StatusBadRequest, fmt.Errorf("action must be restart or rebuild"))
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	action := req.Action
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		buildFailed := a.HasSource() && a.Spec.Image == "" && meta.IsStatusConditionFalse(a.Status.Conditions, shpyrdv1.ConditionBuilt)
		if action == "" {
			action = "restart"
			if buildFailed {
				action = "rebuild"
			}
		}
		if action == "rebuild" && (!a.HasSource() || a.Spec.Image != "") {
			return fmt.Errorf("nothing to build: the project runs a pinned image")
		}
		if action == "restart" && a.CurrentRelease() == nil {
			return fmt.Errorf("nothing to restart: no release yet")
		}
		if action == "restart" && a.Status.Phase == shpyrdv1.PhaseBuilding {
			return fmt.Errorf("a build is running; wait for it to finish")
		}
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		if action == "rebuild" {
			a.Annotations[shpyrdv1.AnnotationRebuildAt] = now
		} else {
			a.Annotations[shpyrdv1.AnnotationRestartedAt] = now
		}
		return nil
	})
	if err != nil {
		return
	}
	msg := "Restarting the instances of the current release"
	if cur := app.CurrentRelease(); cur != nil && action == "restart" {
		msg = fmt.Sprintf("Restarting the instances of v%d", cur.Number)
	}
	if action == "rebuild" {
		msg = "Building the same source again"
	}
	s.audit(c, app.Name, "redeploy", app.Name, action)
	c.JSON(http.StatusOK, RedeployResult{Action: action, Message: msg, App: summarize(app)})
}

// restoreSizes puts back the instance sizes a release ran with.
func restoreSizes(a *shpyrdv1.App, rel *shpyrdv1.Release) {
	if len(rel.Sizes) == 0 {
		return
	}
	if a.Spec.Processes == nil {
		a.Spec.Processes = map[string]shpyrdv1.Process{}
	}
	for proc, size := range rel.Sizes {
		p, ok := a.Spec.Processes[proc]
		if !ok && proc != "web" {
			continue
		}
		if size != "custom" {
			p.Size = size
			p.Resources = corev1.ResourceRequirements{}
		}
		a.Spec.Processes[proc] = p
	}
}

// mutateApp applies a read-modify-write with conflict retries and writes the
// HTTP error itself; callers only check err != nil.
func (s *Server) mutateApp(c *gin.Context, mutate func(*shpyrdv1.App) error) (*shpyrdv1.App, error) {
	key := s.projectKey(c)
	for attempt := 0; attempt < 5; attempt++ {
		app := &shpyrdv1.App{}
		if err := s.apps.Get(c.Request.Context(), key, app); err != nil {
			if apierrors.IsNotFound(err) {
				abort(c, http.StatusNotFound, errors.New("project not found"))
			} else {
				abort(c, http.StatusBadGateway, err)
			}
			return nil, err
		}
		if err := mutate(app); err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "rolling out") {
				status = http.StatusConflict
			}
			abort(c, status, err)
			return nil, err
		}
		// The workspace's plan (RFC-0033): checked on the App as it would
		// be stored, whatever the change was.
		if err := s.checkPlan(c.Request.Context(), s.workspace(c), app); err != nil {
			abort(c, http.StatusBadRequest, err)
			return nil, err
		}
		err := s.apps.Update(c.Request.Context(), app)
		if err == nil {
			return app, nil
		}
		if !apierrors.IsConflict(err) {
			abort(c, http.StatusBadGateway, err)
			return nil, err
		}
	}
	err := errors.New("too many conflicts")
	abort(c, http.StatusConflict, err)
	return nil, err
}

// listAllow is GET /api/projects/:slug/allow: who may reach this project
// from inside the cluster.
func (s *Server) listAllow(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	allow := app.EffectiveAllow()
	if allow == nil {
		allow = []shpyrdv1.AllowEntry{}
	}
	c.JSON(http.StatusOK, allow)
}

// setAllow is PUT /api/projects/:slug/allow: replace the allow list.
func (s *Server) setAllow(c *gin.Context) {
	var req []shpyrdv1.AllowEntry
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	// Validate: each entry must be exactly a project or a platform caller.
	for _, e := range req {
		if (e.Project == "" && e.Platform == "") || (e.Project != "" && e.Platform != "") {
			abort(c, http.StatusBadRequest, fmt.Errorf("each allow entry needs exactly one of project or platform"))
			return
		}
		if e.Platform != "" && e.Platform != "actions" && e.Platform != "mcp" {
			abort(c, http.StatusBadRequest, fmt.Errorf("platform must be actions or mcp"))
			return
		}
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		a.Spec.Allow = req
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "allow", app.Name, fmt.Sprintf("%d entries", len(req)))
	c.JSON(http.StatusOK, app.EffectiveAllow())
}

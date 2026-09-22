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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// AppSummary is the list view of an App. Image references are reduced to
// their digest so registry internals never surface in the UI.
type AppSummary struct {
	Name      string                            `json:"name"`
	Namespace string                            `json:"namespace"`
	Phase     string                            `json:"phase"`
	Message   string                            `json:"message,omitempty"`
	URL       string                            `json:"url,omitempty"`
	Digest    string                            `json:"digest,omitempty"`
	Release   int                               `json:"release"`
	Source    string                            `json:"source,omitempty"`
	Processes map[string]shpyrdv1.ProcessStatus `json:"processes,omitempty"`
	CreatedAt time.Time                         `json:"createdAt"`
}

func summarize(a *shpyrdv1.App) AppSummary {
	s := AppSummary{
		Name:      a.Name,
		Namespace: a.Namespace,
		Phase:     a.Status.Phase,
		Message:   a.Status.Message,
		URL:       a.Status.URL,
		Digest:    Digest(a.Status.Image),
		Processes: a.Status.Processes,
		CreatedAt: a.CreationTimestamp.Time,
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
	Name      string                            `json:"name"`
	Namespace string                            `json:"namespace"`
	CreatedAt time.Time                         `json:"createdAt"`
	Spec      AppDetailSpec                     `json:"spec"`
	Status    AppDetailStatus                   `json:"status"`
	Processes map[string]shpyrdv1.ProcessStatus `json:"processes,omitempty"`
}

type AppDetailSpec struct {
	Source      *shpyrdv1.Source            `json:"source,omitempty"`
	PinnedImage string                      `json:"pinnedDigest,omitempty"`
	Processes   map[string]shpyrdv1.Process `json:"processes,omitempty"`
	Env         []corev1.EnvVar             `json:"env,omitempty"`
	Domains     []string                    `json:"domains,omitempty"`
	Build       *shpyrdv1.Build             `json:"build,omitempty"`
}

type AppDetailStatus struct {
	Phase       string             `json:"phase"`
	Message     string             `json:"message,omitempty"`
	Digest      string             `json:"digest,omitempty"`
	URL         string             `json:"url,omitempty"`
	LatestBuild string             `json:"latestBuild,omitempty"`
	Releases    []ReleaseView      `json:"releases"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
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

func releaseKind(desc string) string {
	switch {
	case strings.HasPrefix(desc, "Rollback"):
		return "rollback"
	case strings.HasPrefix(desc, "Set ") || strings.HasPrefix(desc, "Remove ") || strings.HasPrefix(desc, "Config"):
		return "config"
	default:
		return "deploy"
	}
}

func detail(a *shpyrdv1.App, buildByDigest map[string]int) AppDetail {
	d := AppDetail{
		Name:      a.Name,
		Namespace: a.Namespace,
		CreatedAt: a.CreationTimestamp.Time,
		Spec: AppDetailSpec{
			Source:      a.Spec.Source,
			PinnedImage: Digest(a.Spec.Image),
			Processes:   a.Spec.Processes,
			Env:         a.Spec.Env,
			Domains:     a.Spec.Domains,
			Build:       a.Spec.Build,
		},
		Status: AppDetailStatus{
			Phase:       firstNonEmpty(a.Status.Phase, shpyrdv1.PhasePending),
			Message:     a.Status.Message,
			Digest:      Digest(a.Status.Image),
			URL:         a.Status.URL,
			LatestBuild: a.Status.LatestBuild,
			Releases:    []ReleaseView{},
			Conditions:  a.Status.Conditions,
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
	for _, r := range a.Status.Releases {
		dg := Digest(r.Image)
		d.Status.Releases = append(d.Status.Releases, ReleaseView{
			Number: r.Number, Digest: dg, Build: buildByDigest[dg], Source: r.Source, Description: r.Description,
			CreatedAt: r.CreatedAt.Time, Processes: r.Processes, Kind: releaseKind(r.Description),
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
	for i := range list.Items {
		out = append(out, summarize(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	c.JSON(http.StatusOK, out)
}

func (s *Server) loadApp(c *gin.Context) (*shpyrdv1.App, bool) {
	app := &shpyrdv1.App{}
	err := s.apps.Get(c.Request.Context(), types.NamespacedName{Namespace: c.Param("ns"), Name: c.Param("name")}, app)
	if apierrors.IsNotFound(err) {
		abort(c, http.StatusNotFound, errors.New("app not found"))
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

var appNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)

// CreateAppRequest creates an app: namespace app-<name> plus the App.
type CreateAppRequest struct {
	Name      string                      `json:"name" binding:"required"`
	Domains   []string                    `json:"domains,omitempty"`
	Processes map[string]shpyrdv1.Process `json:"processes,omitempty"`
	Git       *shpyrdv1.GitSource         `json:"git,omitempty"`
	SubPath   string                      `json:"subPath,omitempty"`
}

func (s *Server) createApp(c *gin.Context) {
	var req CreateAppRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if !appNameRe.MatchString(req.Name) {
		abort(c, http.StatusBadRequest, errors.New("name must be lowercase letters, digits and dashes (max 40 characters)"))
		return
	}
	ctx := c.Request.Context()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "app-" + req.Name,
		Labels: map[string]string{shpyrdv1.LabelApp: req.Name, shpyrdv1.LabelManagedBy: "shpyrd"},
	}}
	if err := s.apps.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		abort(c, http.StatusBadGateway, fmt.Errorf("create namespace: %w", err))
		return
	}
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: ns.Name},
		Spec:       shpyrdv1.AppSpec{Domains: req.Domains, Processes: req.Processes},
	}
	if req.Git != nil && req.Git.URL != "" {
		app.Spec.Source = &shpyrdv1.Source{Git: req.Git, SubPath: req.SubPath}
	}
	if err := s.apps.Create(ctx, app); err != nil {
		if apierrors.IsAlreadyExists(err) {
			abort(c, http.StatusConflict, fmt.Errorf("app %q already exists", req.Name))
			return
		}
		abort(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusCreated, summarize(app))
}

// DeployRequest points the app at a new source (or prebuilt image).
type DeployRequest struct {
	Git     *shpyrdv1.GitSource `json:"git,omitempty"`
	SubPath string              `json:"subPath,omitempty"`
	Image   string              `json:"image,omitempty"`
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
		return nil
	})
	if err != nil {
		return
	}
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
		p.Replicas = ptr.To(*req.Replicas)
		a.Spec.Processes[req.Process] = p
		return nil
	})
	if err != nil {
		return
	}
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
	c.JSON(http.StatusOK, summarize(app))
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
	key := types.NamespacedName{Namespace: c.Param("ns"), Name: c.Param("name")}
	for attempt := 0; attempt < 5; attempt++ {
		app := &shpyrdv1.App{}
		if err := s.apps.Get(c.Request.Context(), key, app); err != nil {
			if apierrors.IsNotFound(err) {
				abort(c, http.StatusNotFound, errors.New("app not found"))
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

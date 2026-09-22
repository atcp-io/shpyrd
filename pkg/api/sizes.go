package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/sizes"
)

// SizesResponse is the instance size catalog as the dashboard sees it.
type SizesResponse struct {
	Default string       `json:"default"`
	Sizes   []sizes.Size `json:"sizes"`
}

// loadCatalog reads the catalog ConfigMap, falling back to defaults.
func (s *Server) loadCatalog(c *gin.Context) (*sizes.Catalog, *corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	err := s.apps.Get(c.Request.Context(), types.NamespacedName{Namespace: s.kube.Namespace, Name: sizes.ConfigMapName}, cm)
	if apierrors.IsNotFound(err) {
		d := sizes.Defaults()
		return &d, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	cat, err := sizes.Parse([]byte(cm.Data[sizes.ConfigMapKey]))
	if err != nil {
		return nil, cm, err
	}
	return cat, cm, nil
}

func (s *Server) getSizes(c *gin.Context) {
	cat, _, err := s.loadCatalog(c)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, SizesResponse{Default: cat.Default, Sizes: cat.Sorted()})
}

// putSizes replaces the whole catalog (the dashboard edits it as a list).
func (s *Server) putSizes(c *gin.Context) {
	var req sizes.Catalog
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if err := req.Validate(); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if err := s.saveCatalog(c, req); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, SizesResponse{Default: req.Default, Sizes: req.Sorted()})
}

func (s *Server) saveCatalog(c *gin.Context, cat sizes.Catalog) error {
	data, err := cat.Marshal()
	if err != nil {
		return err
	}
	ctx := c.Request.Context()
	key := types.NamespacedName{Namespace: s.kube.Namespace, Name: sizes.ConfigMapName}
	cm := &corev1.ConfigMap{}
	if err := s.apps.Get(ctx, key, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
		cm.Data = map[string]string{sizes.ConfigMapKey: string(data)}
		return s.apps.Create(ctx, cm)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[sizes.ConfigMapKey] = string(data)
	return s.apps.Update(ctx, cm)
}

type resizeRequest struct {
	Process string `json:"process" binding:"required"`
	Size    string `json:"size" binding:"required"`
}

// resizeApp sets the instance size of one process type.
func (s *Server) resizeApp(c *gin.Context) {
	var req resizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	cat, _, err := s.loadCatalog(c)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	if _, ok := cat.Get(req.Size); !ok {
		abort(c, http.StatusBadRequest, fmt.Errorf("unknown size %q", req.Size))
		return
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		if a.Spec.Processes == nil {
			a.Spec.Processes = map[string]shpyrdv1.Process{"web": {}}
		}
		p, ok := a.Spec.Processes[req.Process]
		if !ok && req.Process != "web" {
			return errors.New("unknown process " + req.Process)
		}
		p.Size = req.Size
		p.Resources = corev1.ResourceRequirements{} // the size is authoritative
		a.Spec.Processes[req.Process] = p
		return nil
	})
	if err != nil {
		return
	}
	c.JSON(http.StatusOK, summarize(app))
}

// ProcessChange is one entry of an applyProcesses request.
type ProcessChange struct {
	Size     *string `json:"size,omitempty"`
	Replicas *int32  `json:"replicas,omitempty"`
}

type applyProcessesRequest struct {
	Processes map[string]ProcessChange `json:"processes" binding:"required"`
}

// applyProcesses changes sizes and instance counts of several process types
// in one update, so a batch of edits yields a single release and rollout.
func (s *Server) applyProcesses(c *gin.Context) {
	var req applyProcessesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if len(req.Processes) == 0 {
		abort(c, http.StatusBadRequest, errors.New("no changes"))
		return
	}
	cat, _, err := s.loadCatalog(c)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	resizes := false
	for name, ch := range req.Processes {
		if ch.Size != nil {
			if _, ok := cat.Get(*ch.Size); !ok {
				abort(c, http.StatusBadRequest, fmt.Errorf("unknown size %q for %s", *ch.Size, name))
				return
			}
			resizes = true
		}
		if ch.Replicas != nil && (*ch.Replicas < 0 || *ch.Replicas > 100) {
			abort(c, http.StatusBadRequest, errors.New("replicas must be between 0 and 100"))
			return
		}
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		if resizes && rolloutInProgress(a) {
			return fmt.Errorf("a release is still rolling out (%s); wait for it to finish", firstNonEmpty(a.Status.Message, a.Status.Phase))
		}
		if a.Spec.Processes == nil {
			a.Spec.Processes = map[string]shpyrdv1.Process{"web": {}}
		}
		for name, ch := range req.Processes {
			p, ok := a.Spec.Processes[name]
			if !ok && name != "web" {
				return errors.New("unknown process " + name)
			}
			if ch.Size != nil {
				p.Size = *ch.Size
				p.Resources = corev1.ResourceRequirements{}
			}
			if ch.Replicas != nil {
				p.Replicas = ch.Replicas
			}
			a.Spec.Processes[name] = p
		}
		return nil
	})
	if err != nil {
		return
	}
	c.JSON(http.StatusOK, summarize(app))
}

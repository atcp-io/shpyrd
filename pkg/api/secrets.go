package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/configvars"
)

// ConfigVarsResponse lists config var names; values are never returned.
type ConfigVarsResponse struct {
	Vars []configvars.Var `json:"vars"`
	// Bound are read-only variables provided by attached resources
	// (RFC-0003); they win over Vars with the same name.
	Bound []BoundVar `json:"bound,omitempty"`
}

// ConfigVarsUpdate sets and/or unsets config vars. Dotenv is parsed as
// KEY=VALUE lines and merged into Set (bulk paste).
type ConfigVarsUpdate struct {
	Set    map[string]string `json:"set,omitempty"`
	Unset  []string          `json:"unset,omitempty"`
	Dotenv string            `json:"dotenv,omitempty"`
}

// BoundVar is a config var injected by an attached resource.
type BoundVar struct {
	Name     string `json:"name"`
	Provider string `json:"provider"` // "<Kind>/<name>"
}

func (s *Server) appSecretKeys(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	resp := ConfigVarsResponse{Vars: []configvars.Var{}}
	sec := &corev1.Secret{}
	err := s.apps.Get(c.Request.Context(), types.NamespacedName{Namespace: app.Namespace, Name: app.EnvSecretName()}, sec)
	if err != nil && !apierrors.IsNotFound(err) {
		abort(c, http.StatusBadGateway, err)
		return
	}
	if err == nil {
		resp.Vars = configvars.List(sec)
	}
	resp.Bound = s.boundVars(c.Request.Context(), app)
	c.JSON(http.StatusOK, resp)
}

// boundVars lists the variables of <app>-bindings with their providers.
func (s *Server) boundVars(ctx context.Context, app *shpyrdv1.App) []BoundVar {
	sec := &corev1.Secret{}
	if err := s.apps.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.BindingsSecretName()}, sec); err != nil {
		return nil
	}
	providers := map[string]string{}
	_ = json.Unmarshal([]byte(sec.Annotations[shpyrdv1.AnnotationBindingProviders]), &providers)
	out := make([]BoundVar, 0, len(sec.Data))
	for k := range sec.Data {
		out = append(out, BoundVar{Name: k, Provider: providers[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) updateAppSecrets(c *gin.Context) {
	var req ConfigVarsUpdate
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if req.Dotenv != "" {
		parsed, err := configvars.ParseDotenv(req.Dotenv)
		if err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
		if req.Set == nil {
			req.Set = map[string]string{}
		}
		for k, v := range parsed {
			req.Set[k] = v
		}
	}
	if len(req.Set) == 0 && len(req.Unset) == 0 {
		abort(c, http.StatusBadRequest, errors.New("nothing to change"))
		return
	}
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.EnvSecretName()}
	for attempt := 0; attempt < 5; attempt++ {
		sec := &corev1.Secret{}
		create := false
		if err := s.apps.Get(ctx, key, sec); err != nil {
			if !apierrors.IsNotFound(err) {
				abort(c, http.StatusBadGateway, err)
				return
			}
			create = true
			sec = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{shpyrdv1.LabelApp: app.Name}},
				Type:       corev1.SecretTypeOpaque,
			}
		}
		if err := configvars.Apply(sec, req.Set, req.Unset, time.Now()); err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
		var err error
		if create {
			err = s.apps.Create(ctx, sec)
		} else {
			err = s.apps.Update(ctx, sec)
		}
		if err == nil {
			c.JSON(http.StatusOK, ConfigVarsResponse{Vars: configvars.List(sec)})
			return
		}
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			abort(c, http.StatusBadGateway, err)
			return
		}
	}
	abort(c, http.StatusConflict, errors.New("too many conflicts"))
}

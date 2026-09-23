package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/configvars"
)

// Global config vars (RFC-0016): set once by a platform admin, injected into
// every project that does not opt out, write-only like project vars.

// GlobalsResponse lists the global config var names, never their values,
// and how many projects receive them (each gets a release on a change).
type GlobalsResponse struct {
	Vars []configvars.Var `json:"vars"`
	// Projects is the number of projects that receive at least one global.
	Projects int `json:"projects"`
}

func (s *Server) globalsKey() types.NamespacedName {
	return types.NamespacedName{Namespace: s.kube.Namespace, Name: shpyrdv1.GlobalEnvSecretName}
}

// getGlobals is GET /api/globals (cluster.admin).
func (s *Server) getGlobals(c *gin.Context) {
	sec := &corev1.Secret{}
	err := s.apps.Get(c.Request.Context(), s.globalsKey(), sec)
	if err != nil && !apierrors.IsNotFound(err) {
		abort(c, http.StatusBadGateway, err)
		return
	}
	if err != nil {
		sec = nil
	}
	c.JSON(http.StatusOK, GlobalsResponse{Vars: configvars.List(sec), Projects: s.projectsReceivingGlobals(c.Request.Context())})
}

// putGlobals is PUT /api/globals: set and unset names, dotenv for bulk
// paste. The controller mirrors the change into every project and records
// a "Global config change" release for each.
func (s *Server) putGlobals(c *gin.Context) {
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
	ctx := c.Request.Context()
	key := s.globalsKey()
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
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}},
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
			if len(req.Set) > 0 {
				s.audit(c, "", "globals.set", "global config vars", "set "+strings.Join(sortedKeys(req.Set), ", "))
			}
			if len(req.Unset) > 0 {
				s.audit(c, "", "globals.unset", "global config vars", "unset "+strings.Join(req.Unset, ", "))
			}
			c.JSON(http.StatusOK, GlobalsResponse{Vars: configvars.List(sec), Projects: s.projectsReceivingGlobals(ctx)})
			return
		}
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			abort(c, http.StatusBadGateway, err)
			return
		}
	}
	abort(c, http.StatusConflict, errors.New("too many conflicts"))
}

// projectsReceivingGlobals counts the projects that have not opted out.
func (s *Server) projectsReceivingGlobals(ctx context.Context) int {
	var list shpyrdv1.AppList
	if err := s.apps.List(ctx, &list); err != nil {
		return 0
	}
	n := 0
	for _, a := range list.Items {
		if a.Spec.Globals == nil || !a.Spec.Globals.Disabled {
			n++
		}
	}
	return n
}

// globalVars lists the globals a project receives, from its mirror (already
// filtered by the project's opt-out), with when each was set.
func (s *Server) globalVars(ctx context.Context, app *shpyrdv1.App) []configvars.Var {
	sec := &corev1.Secret{}
	if err := s.apps.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: shpyrdv1.GlobalEnvSecretName}, sec); err != nil {
		return nil
	}
	return configvars.List(sec)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

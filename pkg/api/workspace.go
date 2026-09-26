package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"shpyrd/pkg/ext"
	"shpyrd/pkg/store"
)

// The workspace (RFC-0033): the tenant every project belongs to. The
// open-source platform has exactly one, implicit; the API exposes its name
// and the people it has seen sign in, so a "Workspace" page exists before
// the multi-workspace cloud does.

// WorkspaceView is GET /api/workspace.
type WorkspaceView struct {
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	Implicit  bool      `json:"implicit"`
	Domain    string    `json:"domain,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// PersonView is one identity the workspace has seen.
type PersonView struct {
	Email       string    `json:"email"`
	Name        string    `json:"name,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Groups      []string  `json:"groups"`
	Realm       string    `json:"realm"`
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
}

func (s *Server) workspaceView(w *store.Workspace) WorkspaceView {
	return WorkspaceView{Slug: w.Slug, Name: w.Name, Implicit: w.Slug == store.DefaultWorkspace, Domain: s.opts.Public.Domain, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
}

func (s *Server) getWorkspace(c *gin.Context) {
	w, err := s.store.Workspace(c.Request.Context(), s.workspace(c))
	if err != nil {
		storeErr(c, err, "workspace")
		return
	}
	c.JSON(http.StatusOK, s.workspaceView(w))
}

func (s *Server) updateWorkspace(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 80 {
		abort(c, http.StatusBadRequest, errors.New("name must be 1 to 80 characters"))
		return
	}
	w, err := s.store.UpdateWorkspace(c.Request.Context(), s.workspace(c), name)
	if err != nil {
		storeErr(c, err, "workspace")
		return
	}
	s.audit(c, "", "workspace.update", w.Slug, "name: "+name)
	c.JSON(http.StatusOK, s.workspaceView(w))
}

func (s *Server) listPeople(c *gin.Context) {
	ids, err := s.store.ListIdentities(c.Request.Context(), s.workspace(c))
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]PersonView, 0, len(ids))
	for _, id := range ids {
		groups := id.Groups
		if groups == nil {
			groups = []string{}
		}
		out = append(out, PersonView{Email: id.Email, Name: id.Name, Provider: id.Provider, Groups: groups, Realm: id.Realm, FirstSeenAt: id.FirstSeenAt, LastSeenAt: id.LastSeenAt})
	}
	c.JSON(http.StatusOK, out)
}

// forgetPerson removes the record of a person; grants and team memberships
// are by email and stay (remove those explicitly). Sign-in recreates the
// record.
func (s *Server) forgetPerson(c *gin.Context) {
	email := strings.ToLower(c.Param("email"))
	if err := s.store.DeleteIdentity(c.Request.Context(), s.workspace(c), email); err != nil {
		storeErr(c, err, "person")
		return
	}
	s.audit(c, "", "workspace.person.forget", email, "")
	c.Status(http.StatusNoContent)
}

// recordSignIn notes an identity in the workspace's people. Tokens and the
// admin token are not people. Failures are logged, never surfaced: a sign-in
// must not depend on the store.
func (s *Server) recordSignIn(c *gin.Context, id ext.Identity) {
	if id.Email == "" || id.Provider == "token" || id.Provider == "kubeconfig" || id.Subject == "admin-token" {
		return
	}
	if _, err := s.store.TouchIdentity(c.Request.Context(), s.workspace(c), store.Identity{Email: id.Email, Name: id.Name, Provider: id.Provider, Groups: id.Groups}); err != nil {
		s.log.Warn("could not record sign-in", "email", id.Email, "err", err.Error())
	}
}

// exportWorkspace is GET /api/workspace/export: the store's content as the
// platform backup carries it (RFC-0037).
func (s *Server) exportWorkspace(c *gin.Context) {
	dump, err := s.store.Export(c.Request.Context(), s.workspace(c))
	if err != nil {
		storeErr(c, err, "workspace")
		return
	}
	c.JSON(http.StatusOK, dump)
}

// importWorkspace is POST /api/workspace/import[?overwrite=true]: teams,
// grants and people from a backup (`shpyrd cluster restore`).
func (s *Server) importWorkspace(c *gin.Context) {
	var dump store.Dump
	if err := c.ShouldBindJSON(&dump); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if dump.Version > store.DumpVersion {
		abort(c, http.StatusBadRequest, errors.New("this backup was made by a newer shpyrd; upgrade first"))
		return
	}
	res, err := s.store.Import(c.Request.Context(), s.workspace(c), &dump, c.Query("overwrite") == "true")
	if err != nil {
		storeErr(c, err, "workspace")
		return
	}
	s.membershipChanged()
	s.audit(c, "", "workspace.import", "store", "teams "+itoa(res.Teams)+", grants "+itoa(res.Grants)+", people "+itoa(res.Identities))
	c.JSON(http.StatusOK, gin.H{"teams": res.Teams, "grants": res.Grants, "people": res.Identities, "skipped": res.Skipped})
}

func itoa(n int) string { return strconv.Itoa(n) }

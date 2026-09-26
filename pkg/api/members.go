package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/authz"
	"shpyrd/pkg/ext"
	project_ "shpyrd/pkg/project"
	"shpyrd/pkg/store"
	"shpyrd/pkg/tenancy"
)

// Authorization (RFC-0008): every protected route names the action it
// performs; the middleware resolves the caller's roles and refuses with 403
// and a plain explanation. The dashboard reads the same roles from /api/me
// to hide what the role cannot do.

const rolesKey = "shpyrd.roles"

// rolesOf resolves (and caches on the request) the caller's roles.
func (s *Server) rolesOf(c *gin.Context) (authz.Roles, error) {
	if v, ok := c.Get(rolesKey); ok {
		return v.(authz.Roles), nil
	}
	id, ok := ext.IdentityFrom(c)
	if !ok {
		// Authentication disabled: everything is allowed.
		id = ext.Identity{Subject: "admin-token", Provider: "token", Admin: true}
	}
	roles, err := s.authz.RolesIn(c.Request.Context(), s.workspace(c), id)
	if err != nil {
		return authz.Roles{}, err
	}
	c.Set(rolesKey, roles)
	return roles, nil
}

// require refuses the request unless the caller may perform action; project
// actions take the project from the :slug parameter. A malformed slug is a
// 404: no such project can exist.
func (s *Server) require(action authz.Action) gin.HandlerFunc {
	return func(c *gin.Context) {
		project := c.Param("slug")
		if project != "" && (!project_.ValidSlug(project) || strings.HasPrefix(project, "app-")) {
			// Slugs never start with "app-": that is the namespace prefix,
			// so a client passing a namespace here is a bug worth a loud 404.
			abort(c, http.StatusNotFound, errors.New("project not found (paths take the project slug, not its namespace)"))
			return
		}
		roles, err := s.rolesOf(c)
		if err != nil {
			abort(c, http.StatusBadGateway, fmt.Errorf("resolve roles: %w", err))
			return
		}
		if !roles.Can(action, project) {
			abort(c, http.StatusForbidden, denial(roles, action, project))
			return
		}
		c.Next()
	}
}

// denial explains a refusal in the user's terms.
func denial(roles authz.Roles, action authz.Action, project string) error {
	verb := map[authz.Action]string{
		authz.ProjectView: "view", authz.ProjectDeploy: "deploy or roll back", authz.ProjectScale: "scale or resize",
		authz.ProjectConfig: "change config vars of", authz.ProjectExec: "run commands in", authz.ProjectResource: "manage resources of",
		authz.ProjectMembers: "manage members of", authz.ProjectDestroy: "destroy",
		authz.ClusterView: "view the cluster", authz.ClusterAdmin: "administer the cluster", authz.ClusterCreate: "create projects",
	}[action]
	if project == "" || strings.HasPrefix(string(action), "cluster.") {
		return fmt.Errorf("your role cannot %s (needs a platform role)", verb)
	}
	if role := roles.ProjectRole(project); role != "" {
		return fmt.Errorf("your role on project %s is %s: it cannot %s the project", project, role, verb)
	}
	return fmt.Errorf("you have no access to project %s", project)
}

// canView filters lists to the projects the caller may see.
func (s *Server) canView(c *gin.Context, project string) bool {
	roles, err := s.rolesOf(c)
	return err == nil && roles.Can(authz.ProjectView, project)
}

// ---- teams ----------------------------------------------------------------

// TeamView is a team as returned by the API.
type TeamView struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Members      []string `json:"members"`
	Groups       []string `json:"groups"`
	PlatformRole string   `json:"platformRole,omitempty"`
	// Everyone marks the built-in team of every person who signed in.
	Everyone bool `json:"everyone,omitempty"`
}

func teamView(t store.Team) TeamView {
	v := TeamView{Name: t.Name, Description: t.Description, Members: t.Members, Groups: t.Groups, PlatformRole: t.PlatformRole, Everyone: t.Everyone}
	if v.Members == nil {
		v.Members = []string{}
	}
	if v.Groups == nil {
		v.Groups = []string{}
	}
	return v
}

// TeamRequest creates or replaces a team.
type TeamRequest struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Members      []string `json:"members"`
	Groups       []string `json:"groups"`
	PlatformRole string   `json:"platformRole,omitempty"`
}

var dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ctxWorkspace is the gin context key of the resolved workspace.
const ctxWorkspace = "shpyrd.workspace"

// tenant is the workspace the request's host belongs to (RFC-0033 phase
// 6), resolved once per request. The open-source platform resolves every
// host to the implicit workspace; a multi-workspace resolver may answer
// tenancy.ErrUnknownHost.
func (s *Server) tenant(c *gin.Context) (*store.Workspace, error) {
	if v, ok := c.Get(ctxWorkspace); ok {
		return v.(*store.Workspace), nil
	}
	ws, err := s.tenancy.Resolve(c.Request.Context(), c.Request.Host)
	if err != nil {
		return nil, err
	}
	c.Set(ctxWorkspace, ws)
	return ws, nil
}

// workspace is the slug the request is scoped to. Routes behind
// requireTenant always have one; elsewhere an unresolvable host yields ""
// and every store call answers not found.
func (s *Server) workspace(c *gin.Context) string {
	ws, err := s.tenant(c)
	if err != nil {
		return ""
	}
	return ws.Slug
}

// workspaceID is the id of the request's workspace, "" when unresolved.
func (s *Server) workspaceID(c *gin.Context) string {
	ws, err := s.tenant(c)
	if err != nil {
		return ""
	}
	return ws.ID
}

// requireTenant answers for hosts no workspace claims and for suspended
// workspaces, so handlers behind it can count on s.tenant(c).
func (s *Server) requireTenant() gin.HandlerFunc {
	return func(c *gin.Context) {
		ws, err := s.tenant(c)
		asJSON := strings.HasPrefix(c.Request.URL.Path, "/api/") || wantsJSON(c)
		switch {
		case errors.Is(err, tenancy.ErrUnknownHost):
			if asJSON {
				abort(c, http.StatusNotFound, fmt.Errorf("no workspace answers at %s", tenancy.Host(c.Request.Host)))
			} else {
				s.edgePage(c, http.StatusNotFound, "Nothing here", "No workspace answers at this address.", nil)
				c.Abort()
			}
			return
		case err != nil:
			abort(c, http.StatusBadGateway, fmt.Errorf("resolve workspace: %w", err))
			return
		case ws.Status == store.WorkspaceSuspended:
			if asJSON {
				abort(c, http.StatusForbidden, errors.New("this workspace is suspended"))
			} else {
				s.edgePage(c, http.StatusForbidden, "Workspace suspended", "This workspace has been switched off by the platform operator.", nil)
				c.Abort()
			}
			return
		}
		c.Next()
	}
}

// storeErr maps store errors to HTTP statuses.
func storeErr(c *gin.Context, err error, what string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		abort(c, http.StatusNotFound, fmt.Errorf("%s not found", what))
	case errors.Is(err, store.ErrConflict):
		abort(c, http.StatusConflict, fmt.Errorf("this %s already exists", what))
	case errors.Is(err, store.ErrBuiltIn):
		abort(c, http.StatusBadRequest, fmt.Errorf("the %s team is built in: every person who signs in belongs to it; grant it roles, but it cannot be edited or deleted", store.TeamEveryone))
	default:
		abort(c, http.StatusBadGateway, err)
	}
}

// membershipChanged refreshes the authz cache and the RBAC mirror.
func (s *Server) membershipChanged() {
	s.authz.Invalidate()
	if s.opts.MembershipChanged != nil {
		s.opts.MembershipChanged()
	}
}

func (s *Server) listTeams(c *gin.Context) {
	teams, err := s.store.ListTeams(c.Request.Context(), s.workspace(c))
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]TeamView, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamView(t))
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) putTeam(c *gin.Context) {
	var req TeamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	name := firstNonEmpty(c.Param("name"), req.Name)
	if !dnsName.MatchString(name) {
		abort(c, http.StatusBadRequest, errors.New("team names use lowercase letters, digits and dashes"))
		return
	}
	switch req.PlatformRole {
	case "", shpyrdv1.RolePlatformAdmin, shpyrdv1.RolePlatformViewer:
	default:
		abort(c, http.StatusBadRequest, errors.New("platformRole must be platform-admin, platform-viewer or empty"))
		return
	}
	members, err := normalizeEmails(req.Members)
	if err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	team, created, err := s.store.PutTeam(c.Request.Context(), s.workspace(c), store.Team{Name: name, Description: req.Description, Members: members, Groups: compact(req.Groups), PlatformRole: req.PlatformRole})
	if err != nil {
		storeErr(c, err, "team")
		return
	}
	s.membershipChanged()
	s.audit(c, "", "team."+map[bool]string{true: "create", false: "update"}[created], name, "")
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, teamView(*team))
}

func (s *Server) deleteTeam(c *gin.Context) {
	name := c.Param("name")
	// Grants given to the team go with it.
	if err := s.store.DeleteTeam(c.Request.Context(), s.workspace(c), name); err != nil {
		storeErr(c, err, "team")
		return
	}
	s.membershipChanged()
	s.audit(c, "", "team.delete", name, "")
	c.Status(http.StatusNoContent)
}

// ---- project members -------------------------------------------------------

// MemberView is one grant on a project.
type MemberView struct {
	Name    string `json:"name"`
	ID      string `json:"id,omitempty"`
	Project string `json:"project"`
	Role    string `json:"role"`
	User    string `json:"user,omitempty"`
	Team    string `json:"team,omitempty"`
}

// MemberRequest grants a role to a user or a team.
type MemberRequest struct {
	Role string `json:"role" binding:"required"`
	User string `json:"user,omitempty"`
	Team string `json:"team,omitempty"`
}

func memberView(g store.Grant) MemberView {
	return MemberView{Name: MemberName(g.Project, g.Role, g.User, g.Team), ID: g.ID, Project: g.Project, Role: g.Role, User: g.User, Team: g.Team}
}

// MemberName is the deterministic name of a grant, kept from the days
// grants were objects: the CLI and the dashboard remove grants by it.
func MemberName(project, role, user, team string) string {
	subject := "user-" + strings.NewReplacer("@", "-at-", ".", "-", "+", "-plus-", "_", "-").Replace(strings.ToLower(user))
	if team != "" {
		subject = "team-" + team
	}
	name := project + "-" + role + "-" + subject
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

func (s *Server) listMembers(c *gin.Context) {
	project := c.Param("slug")
	grants, err := s.store.ListProjectGrants(c.Request.Context(), s.workspace(c), project)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := []MemberView{}
	for _, g := range grants {
		out = append(out, memberView(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	c.JSON(http.StatusOK, out)
}

// listAllMembers lists every grant of the workspace (`shpyrd members list`).
func (s *Server) listAllMembers(c *gin.Context) {
	grants, err := s.store.ListGrants(c.Request.Context(), s.workspace(c))
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := []MemberView{}
	for _, g := range grants {
		out = append(out, memberView(g))
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) addMember(c *gin.Context) {
	var req MemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	project := c.Param("slug")
	switch req.Role {
	case shpyrdv1.RoleUser, shpyrdv1.RoleViewer, shpyrdv1.RoleDeveloper, shpyrdv1.RoleAdmin:
	default:
		abort(c, http.StatusBadRequest, errors.New("role must be user, viewer, developer or admin"))
		return
	}
	if (req.User == "") == (req.Team == "") {
		abort(c, http.StatusBadRequest, errors.New("give exactly one of user (email) or team"))
		return
	}
	if req.User != "" {
		emails, err := normalizeEmails([]string{req.User})
		if err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
		req.User = emails[0]
	}
	g, err := s.store.AddGrant(c.Request.Context(), s.workspace(c), store.Grant{Project: project, Role: req.Role, User: req.User, Team: req.Team})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			abort(c, http.StatusNotFound, errors.New("team not found"))
			return
		}
		storeErr(c, err, "grant")
		return
	}
	s.membershipChanged()
	s.audit(c, project, "member.add", firstNonEmpty(req.User, "team "+req.Team), req.Role)
	c.JSON(http.StatusCreated, memberView(*g))
}

// removeMember accepts the grant's id or its deterministic name.
func (s *Server) removeMember(c *gin.Context) {
	project, key := c.Param("slug"), c.Param("name")
	ctx := c.Request.Context()
	grants, err := s.store.ListProjectGrants(ctx, s.workspace(c), project)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	for _, g := range grants {
		if g.ID != key && MemberName(g.Project, g.Role, g.User, g.Team) != key {
			continue
		}
		if err := s.store.DeleteGrant(ctx, s.workspace(c), g.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			abort(c, http.StatusBadGateway, err)
			return
		}
		s.membershipChanged()
		s.audit(c, project, "member.remove", firstNonEmpty(g.User, "team "+g.Team), g.Role)
		c.Status(http.StatusNoContent)
		return
	}
	abort(c, http.StatusNotFound, errors.New("member not found"))
}

func normalizeEmails(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.Contains(e, "@") {
			return nil, fmt.Errorf("%q is not an email address", e)
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out, nil
}

func compact(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

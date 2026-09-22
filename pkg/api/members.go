package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/authz"
	"shpyrd/pkg/ext"
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
	roles, err := s.authz.Roles(c.Request.Context(), id)
	if err != nil {
		return authz.Roles{}, err
	}
	c.Set(rolesKey, roles)
	return roles, nil
}

// require refuses the request unless the caller may perform action; project
// actions take the project from the :ns parameter.
func (s *Server) require(action authz.Action) gin.HandlerFunc {
	return func(c *gin.Context) {
		roles, err := s.rolesOf(c)
		if err != nil {
			abort(c, http.StatusBadGateway, fmt.Errorf("resolve roles: %w", err))
			return
		}
		project := authz.ProjectFromNamespace(c.Param("ns"))
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
}

func teamView(t shpyrdv1.Team) TeamView {
	v := TeamView{Name: t.Name, Description: t.Spec.Description, Members: t.Spec.Members, Groups: t.Spec.Groups, PlatformRole: t.Spec.PlatformRole}
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

func (s *Server) listTeams(c *gin.Context) {
	var list shpyrdv1.TeamList
	if err := s.apps.List(c.Request.Context(), &list); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]TeamView, 0, len(list.Items))
	for _, t := range list.Items {
		out = append(out, teamView(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
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
	team := &shpyrdv1.Team{ObjectMeta: metav1.ObjectMeta{Name: name}}
	ctx := c.Request.Context()
	err = s.apps.Get(ctx, types.NamespacedName{Name: name}, team)
	created := apierrors.IsNotFound(err)
	if err != nil && !created {
		abort(c, http.StatusBadGateway, err)
		return
	}
	team.Spec = shpyrdv1.TeamSpec{Description: req.Description, Members: members, Groups: compact(req.Groups), PlatformRole: req.PlatformRole}
	if created {
		team.Labels = map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}
		err = s.apps.Create(ctx, team)
	} else {
		err = s.apps.Update(ctx, team)
	}
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	s.authz.Invalidate()
	s.audit(c, "", "team."+map[bool]string{true: "create", false: "update"}[created], name, "")
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, teamView(*team))
}

func (s *Server) deleteTeam(c *gin.Context) {
	team := &shpyrdv1.Team{ObjectMeta: metav1.ObjectMeta{Name: c.Param("name")}}
	if err := s.apps.Delete(c.Request.Context(), team); err != nil {
		abortNotFound(c, err, "team")
		return
	}
	// Memberships granted to the team go with it.
	var members shpyrdv1.ProjectMemberList
	if err := s.apps.List(c.Request.Context(), &members); err == nil {
		for i := range members.Items {
			if members.Items[i].Spec.Team == team.Name {
				_ = s.apps.Delete(c.Request.Context(), &members.Items[i])
			}
		}
	}
	s.authz.Invalidate()
	s.audit(c, "", "team.delete", team.Name, "")
	c.Status(http.StatusNoContent)
}

// ---- project members -------------------------------------------------------

// MemberView is one grant on a project.
type MemberView struct {
	Name    string `json:"name"`
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

func memberView(m shpyrdv1.ProjectMember) MemberView {
	return MemberView{Name: m.Name, Project: m.Spec.Project, Role: m.Spec.Role, User: m.Spec.User, Team: m.Spec.Team}
}

// MemberName is the deterministic object name of a grant.
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
	project := authz.ProjectFromNamespace(c.Param("ns"))
	var list shpyrdv1.ProjectMemberList
	if err := s.apps.List(c.Request.Context(), &list); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := []MemberView{}
	for _, m := range list.Items {
		if m.Spec.Project == project {
			out = append(out, memberView(m))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	c.JSON(http.StatusOK, out)
}

func (s *Server) addMember(c *gin.Context) {
	var req MemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	project := authz.ProjectFromNamespace(c.Param("ns"))
	switch req.Role {
	case shpyrdv1.RoleViewer, shpyrdv1.RoleDeveloper, shpyrdv1.RoleAdmin:
	default:
		abort(c, http.StatusBadRequest, errors.New("role must be viewer, developer or admin"))
		return
	}
	if (req.User == "") == (req.Team == "") {
		abort(c, http.StatusBadRequest, errors.New("give exactly one of user (email) or team"))
		return
	}
	ctx := c.Request.Context()
	if req.User != "" {
		emails, err := normalizeEmails([]string{req.User})
		if err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
		req.User = emails[0]
	} else {
		team := &shpyrdv1.Team{}
		if err := s.apps.Get(ctx, types.NamespacedName{Name: req.Team}, team); err != nil {
			abortNotFound(c, err, "team")
			return
		}
	}
	m := &shpyrdv1.ProjectMember{
		ObjectMeta: metav1.ObjectMeta{Name: MemberName(project, req.Role, req.User, req.Team), Labels: map[string]string{shpyrdv1.LabelProject: project, shpyrdv1.LabelManagedBy: "shpyrd"}},
		Spec:       shpyrdv1.ProjectMemberSpec{Project: project, Role: req.Role, User: req.User, Team: req.Team},
	}
	if err := s.apps.Create(ctx, m); err != nil {
		if apierrors.IsAlreadyExists(err) {
			abort(c, http.StatusConflict, errors.New("this grant already exists"))
		} else {
			abort(c, http.StatusBadGateway, err)
		}
		return
	}
	s.authz.Invalidate()
	s.audit(c, project, "member.add", firstNonEmpty(req.User, "team "+req.Team), req.Role)
	c.JSON(http.StatusCreated, memberView(*m))
}

func (s *Server) removeMember(c *gin.Context) {
	project := authz.ProjectFromNamespace(c.Param("ns"))
	m := &shpyrdv1.ProjectMember{}
	if err := s.apps.Get(c.Request.Context(), types.NamespacedName{Name: c.Param("name")}, m); err != nil {
		abortNotFound(c, err, "member")
		return
	}
	if m.Spec.Project != project {
		abort(c, http.StatusNotFound, errors.New("member not found"))
		return
	}
	if err := s.apps.Delete(c.Request.Context(), m); client.IgnoreNotFound(err) != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	s.authz.Invalidate()
	s.audit(c, project, "member.remove", firstNonEmpty(m.Spec.User, "team "+m.Spec.Team), m.Spec.Role)
	c.Status(http.StatusNoContent)
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

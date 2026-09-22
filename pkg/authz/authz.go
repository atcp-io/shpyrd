// Package authz decides what an identity may do (RFC-0008): a small static
// role table, memberships read from Team and ProjectMember objects, and a
// bootstrap rule so a fresh cluster is usable before anyone defines teams.
package authz

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/ext"
)

// Action is something the API can do; the role table says who may.
type Action string

// Actions. Project actions take a project; cluster actions do not.
const (
	ProjectView     Action = "project.view"     // overview, releases, builds, logs, metrics, config var names
	ProjectDeploy   Action = "project.deploy"   // deploy, rollback
	ProjectScale    Action = "project.scale"    // scale, resize, process changes
	ProjectConfig   Action = "project.config"   // set/unset config vars
	ProjectExec     Action = "project.exec"     // shell, one-off commands
	ProjectResource Action = "project.resource" // volumes and other resources, attach/detach
	ProjectMembers  Action = "project.members"  // manage members
	ProjectDestroy  Action = "project.destroy"

	ClusterView   Action = "cluster.view"   // cluster page, list projects
	ClusterAdmin  Action = "cluster.admin"  // size catalog, extensions, users, teams, create projects
	ClusterCreate Action = "cluster.create" // create a project (platform admins)
)

// roleActions is the static table: what each role may do.
var roleActions = map[string][]Action{
	shpyrdv1.RoleViewer:    {ProjectView},
	shpyrdv1.RoleDeveloper: {ProjectView, ProjectDeploy, ProjectScale, ProjectConfig, ProjectExec},
	shpyrdv1.RoleAdmin:     {ProjectView, ProjectDeploy, ProjectScale, ProjectConfig, ProjectExec, ProjectResource, ProjectMembers, ProjectDestroy},
	// Platform roles imply a project role on every project.
	shpyrdv1.RolePlatformViewer: {ClusterView, ProjectView},
	shpyrdv1.RolePlatformAdmin: {ClusterView, ClusterAdmin, ClusterCreate,
		ProjectView, ProjectDeploy, ProjectScale, ProjectConfig, ProjectExec, ProjectResource, ProjectMembers, ProjectDestroy},
}

// Roles of an identity: a platform role (or "") and a role per project.
type Roles struct {
	Platform string            `json:"platform,omitempty"`
	Projects map[string]string `json:"projects,omitempty"`
	// Enforced is false while the cluster has no Team or ProjectMember:
	// then every signed-in user is a platform admin (bootstrap).
	Enforced bool `json:"enforced"`
}

// Can reports whether the roles allow an action; project is "" for cluster
// actions.
func (r Roles) Can(action Action, project string) bool {
	if allows(r.Platform, action) {
		return true
	}
	if project == "" {
		return false
	}
	return allows(r.Projects[project], action)
}

// ProjectRole returns the effective role on a project (platform roles map
// to admin/viewer).
func (r Roles) ProjectRole(project string) string {
	switch r.Platform {
	case shpyrdv1.RolePlatformAdmin:
		return shpyrdv1.RoleAdmin
	}
	if role := r.Projects[project]; role != "" {
		return role
	}
	if r.Platform == shpyrdv1.RolePlatformViewer {
		return shpyrdv1.RoleViewer
	}
	return ""
}

func allows(role string, action Action) bool {
	for _, a := range roleActions[role] {
		if a == action {
			return true
		}
	}
	return false
}

// rank orders project roles so the strongest grant wins.
func rank(role string) int {
	switch role {
	case shpyrdv1.RoleAdmin:
		return 3
	case shpyrdv1.RoleDeveloper:
		return 2
	case shpyrdv1.RoleViewer:
		return 1
	}
	return 0
}

func platformRank(role string) int {
	switch role {
	case shpyrdv1.RolePlatformAdmin:
		return 2
	case shpyrdv1.RolePlatformViewer:
		return 1
	}
	return 0
}

// Snapshot is the membership state at one point in time.
type Snapshot struct {
	Teams   []shpyrdv1.Team
	Members []shpyrdv1.ProjectMember
}

// Enforced reports whether any membership object exists.
func (s *Snapshot) Enforced() bool { return len(s.Teams) > 0 || len(s.Members) > 0 }

// teamsOf returns the teams an identity belongs to (by email or group).
func (s *Snapshot) teamsOf(id ext.Identity) map[string]*shpyrdv1.Team {
	out := map[string]*shpyrdv1.Team{}
	email := strings.ToLower(id.Email)
	for i := range s.Teams {
		t := &s.Teams[i]
		if email != "" {
			for _, m := range t.Spec.Members {
				if strings.EqualFold(m, email) {
					out[t.Name] = t
				}
			}
		}
		for _, g := range t.Spec.Groups {
			for _, have := range id.Groups {
				if g == have {
					out[t.Name] = t
				}
			}
		}
	}
	return out
}

// RolesFor resolves an identity's roles. The admin token is always a
// platform admin; without any membership objects everyone is (bootstrap).
func (s *Snapshot) RolesFor(id ext.Identity) Roles {
	r := Roles{Projects: map[string]string{}, Enforced: s.Enforced()}
	// The admin token, and sessions opened from the CLI with cluster access
	// (login tickets), are platform admins: their holders already are.
	if id.Provider == "token" || id.Provider == "kubeconfig" || id.Subject == "admin-token" {
		r.Platform = shpyrdv1.RolePlatformAdmin
		return r
	}
	if !r.Enforced {
		r.Platform = shpyrdv1.RolePlatformAdmin
		return r
	}
	teams := s.teamsOf(id)
	for _, t := range teams {
		if platformRank(t.Spec.PlatformRole) > platformRank(r.Platform) {
			r.Platform = t.Spec.PlatformRole
		}
	}
	email := strings.ToLower(id.Email)
	for _, m := range s.Members {
		match := (m.Spec.User != "" && email != "" && strings.EqualFold(m.Spec.User, email)) ||
			(m.Spec.Team != "" && teams[m.Spec.Team] != nil)
		if !match {
			continue
		}
		if rank(m.Spec.Role) > rank(r.Projects[m.Spec.Project]) {
			r.Projects[m.Spec.Project] = m.Spec.Role
		}
	}
	return r
}

// Load reads the membership objects.
func Load(ctx context.Context, c client.Client) (*Snapshot, error) {
	var teams shpyrdv1.TeamList
	if err := c.List(ctx, &teams); err != nil {
		return nil, err
	}
	var members shpyrdv1.ProjectMemberList
	if err := c.List(ctx, &members); err != nil {
		return nil, err
	}
	sort.Slice(teams.Items, func(i, j int) bool { return teams.Items[i].Name < teams.Items[j].Name })
	return &Snapshot{Teams: teams.Items, Members: members.Items}, nil
}

// Resolver caches snapshots briefly: a request costs two list calls at most
// every TTL.
type Resolver struct {
	Client client.Client
	TTL    time.Duration

	mu      sync.Mutex
	snap    *Snapshot
	fetched time.Time
	now     func() time.Time
}

// Snapshot returns a recent membership snapshot.
func (r *Resolver) Snapshot(ctx context.Context) (*Snapshot, error) {
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	ttl := r.TTL
	if ttl == 0 {
		ttl = 5 * time.Second
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snap != nil && now().Sub(r.fetched) < ttl {
		return r.snap, nil
	}
	snap, err := Load(ctx, r.Client)
	if err != nil {
		if r.snap != nil {
			return r.snap, nil // stale beats down
		}
		return nil, err
	}
	r.snap, r.fetched = snap, now()
	return snap, nil
}

// Invalidate drops the cache (after membership changes through the API).
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	r.snap = nil
	r.mu.Unlock()
}

// Roles resolves an identity through the cache.
func (r *Resolver) Roles(ctx context.Context, id ext.Identity) (Roles, error) {
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return Roles{}, err
	}
	return snap.RolesFor(id), nil
}

// ProjectFromNamespace maps app-<name> to <name>.
func ProjectFromNamespace(ns string) string { return strings.TrimPrefix(ns, "app-") }

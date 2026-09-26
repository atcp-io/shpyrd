package authz

import (
	"testing"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/store"
)

func team(name string, members, groups []string, platform string) store.Team {
	return store.Team{Name: name, Members: members, Groups: groups, PlatformRole: platform}
}

func member(project, role, user, teamName string) store.Grant {
	return store.Grant{Project: project, Role: role, User: user, Team: teamName}
}

func TestBootstrapAndToken(t *testing.T) {
	empty := &Snapshot{}
	r := empty.RolesFor(ext.Identity{Email: "ada@example.test", Provider: "local"})
	if r.Enforced || r.Platform != shpyrdv1.RolePlatformAdmin || !r.Can(ClusterAdmin, "") || !r.Can(ProjectDestroy, "x") {
		t.Errorf("without memberships everyone is a platform admin: %+v", r)
	}
	enforced := &Snapshot{Teams: []store.Team{team("ops", []string{"ops@example.test"}, nil, shpyrdv1.RolePlatformAdmin)}}
	tok := enforced.RolesFor(ext.Identity{Subject: "admin-token", Provider: "token"})
	if tok.Platform != shpyrdv1.RolePlatformAdmin || !tok.Enforced {
		t.Errorf("token must stay platform admin: %+v", tok)
	}
	nobody := enforced.RolesFor(ext.Identity{Email: "new@example.test", Provider: "local"})
	if nobody.Platform != "" || nobody.Can(ProjectView, "hello") || nobody.Can(ClusterView, "") {
		t.Errorf("unknown users get nothing once enforced: %+v", nobody)
	}
}

func TestRolesResolution(t *testing.T) {
	snap := &Snapshot{
		Teams: []store.Team{
			team("ops", []string{"Ops@Example.test"}, nil, shpyrdv1.RolePlatformAdmin),
			team("auditors", nil, []string{"security"}, shpyrdv1.RolePlatformViewer),
			team("web", []string{"dev@example.test"}, []string{"engineering"}, ""),
		},
		Grants: []store.Grant{
			member("shop", shpyrdv1.RoleDeveloper, "", "web"),
			member("shop", shpyrdv1.RoleViewer, "dev@example.test", ""), // weaker grant does not downgrade
			member("blog", shpyrdv1.RoleAdmin, "dev@example.test", ""),
			member("shop", shpyrdv1.RoleViewer, "guest@example.test", ""),
		},
	}
	dev := snap.RolesFor(ext.Identity{Email: "dev@example.test", Provider: "local"})
	if dev.Platform != "" || dev.Projects["shop"] != shpyrdv1.RoleDeveloper || dev.Projects["blog"] != shpyrdv1.RoleAdmin {
		t.Errorf("dev = %+v", dev)
	}
	if !dev.Can(ProjectDeploy, "shop") || dev.Can(ProjectDestroy, "shop") || !dev.Can(ProjectDestroy, "blog") || dev.Can(ClusterView, "") || dev.Can(ProjectView, "other") {
		t.Errorf("dev permissions wrong: %+v", dev)
	}
	if dev.ProjectRole("shop") != shpyrdv1.RoleDeveloper || dev.ProjectRole("other") != "" {
		t.Errorf("project roles: %s %s", dev.ProjectRole("shop"), dev.ProjectRole("other"))
	}

	byGroup := snap.RolesFor(ext.Identity{Email: "someone@example.test", Groups: []string{"engineering"}, Provider: "okta"})
	if byGroup.Projects["shop"] != shpyrdv1.RoleDeveloper {
		t.Errorf("group membership should grant the team's role: %+v", byGroup)
	}

	ops := snap.RolesFor(ext.Identity{Email: "ops@example.test", Provider: "local"})
	if ops.Platform != shpyrdv1.RolePlatformAdmin || !ops.Can(ProjectDestroy, "anything") || !ops.Can(ClusterAdmin, "") || ops.ProjectRole("shop") != shpyrdv1.RoleAdmin {
		t.Errorf("ops = %+v", ops)
	}
	auditor := snap.RolesFor(ext.Identity{Email: "sec@example.test", Groups: []string{"security"}, Provider: "okta"})
	if auditor.Platform != shpyrdv1.RolePlatformViewer || !auditor.Can(ProjectView, "shop") || auditor.Can(ProjectDeploy, "shop") || !auditor.Can(ClusterView, "") || auditor.Can(ClusterAdmin, "") {
		t.Errorf("auditor = %+v", auditor)
	}
	if auditor.ProjectRole("shop") != shpyrdv1.RoleViewer {
		t.Errorf("platform viewer is a viewer everywhere, got %q", auditor.ProjectRole("shop"))
	}
	guest := snap.RolesFor(ext.Identity{Email: "guest@example.test", Provider: "local"})
	if !guest.Can(ProjectView, "shop") || guest.Can(ProjectConfig, "shop") || guest.Can(ProjectExec, "shop") {
		t.Errorf("viewer = %+v", guest)
	}
	if ProjectFromNamespace("app-shop") != "shop" {
		t.Error("namespace mapping")
	}
}

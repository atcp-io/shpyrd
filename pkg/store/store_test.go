package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// The same behaviour for every implementation. Postgres runs when
// SHPYRD_TEST_DATABASE_URL points at an empty database (CI provides one).
func implementations(t *testing.T) map[string]func(t *testing.T) Store {
	impls := map[string]func(t *testing.T) Store{
		"memory": func(t *testing.T) Store { return NewMemory() },
	}
	if url := os.Getenv("SHPYRD_TEST_DATABASE_URL"); url != "" {
		impls["postgres"] = func(t *testing.T) Store {
			ctx := context.Background()
			p, err := Open(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			// A clean slate per test.
			for _, stmt := range []string{"DROP TABLE IF EXISTS grants", "DROP TABLE IF EXISTS teams", "DROP TABLE IF EXISTS identities", "DROP TABLE IF EXISTS workspaces", "DROP TABLE IF EXISTS schema_migrations"} {
				if _, err := p.pool.Exec(ctx, stmt); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Migrate(ctx, "test platform"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(p.Close)
			return p
		}
	}
	return impls
}

func TestStoreConformance(t *testing.T) {
	for name, open := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := open(t)
			if err := s.Migrate(ctx, "ignored on second run"); err != nil {
				t.Fatal(err)
			}

			// The implicit workspace exists and can be renamed.
			w, err := s.Workspace(ctx, DefaultWorkspace)
			if err != nil || w.Slug != DefaultWorkspace || w.Name == "" {
				t.Fatalf("workspace: %+v %v", w, err)
			}
			if _, err := s.Workspace(ctx, "nope"); !errors.Is(err, ErrNotFound) {
				t.Errorf("unknown workspace: %v", err)
			}
			if w, err := s.UpdateWorkspace(ctx, DefaultWorkspace, "Acme"); err != nil || w.Name != "Acme" {
				t.Errorf("rename: %+v %v", w, err)
			}

			// Teams: create, update in place, normalised emails.
			team, created, err := s.PutTeam(ctx, DefaultWorkspace, Team{Name: "platform", Description: "ops", Members: []string{" Ops@Example.test ", "ops@example.test", "dev@example.test"}, Groups: []string{"g1", "g1"}, PlatformRole: "platform-admin"})
			if err != nil || !created || team.ID == "" || len(team.Members) != 2 || team.Members[0] != "ops@example.test" || len(team.Groups) != 1 {
				t.Fatalf("put team: %+v created=%v %v", team, created, err)
			}
			team2, created, err := s.PutTeam(ctx, DefaultWorkspace, Team{Name: "platform", Members: []string{"ops@example.test"}})
			if err != nil || created || team2.ID != team.ID || len(team2.Members) != 1 || team2.PlatformRole != "" {
				t.Fatalf("update team: %+v created=%v %v", team2, created, err)
			}
			if _, _, err := s.PutTeam(ctx, DefaultWorkspace, Team{Name: "finance", Members: []string{"fin@example.test"}}); err != nil {
				t.Fatal(err)
			}
			teams, err := s.ListTeams(ctx, DefaultWorkspace)
			if err != nil || len(teams) != 2 || teams[0].Name != "finance" {
				t.Fatalf("list teams: %+v %v", teams, err)
			}
			if _, err := s.GetTeam(ctx, DefaultWorkspace, "nope"); !errors.Is(err, ErrNotFound) {
				t.Errorf("get unknown team: %v", err)
			}

			// Grants: by user and by team, conflicts, unknown team.
			g1, err := s.AddGrant(ctx, DefaultWorkspace, Grant{Project: "shop", Role: "developer", User: "Dev@Example.test"})
			if err != nil || g1.User != "dev@example.test" || g1.ID == "" {
				t.Fatalf("add grant: %+v %v", g1, err)
			}
			if _, err := s.AddGrant(ctx, DefaultWorkspace, Grant{Project: "shop", Role: "developer", User: "dev@example.test"}); !errors.Is(err, ErrConflict) {
				t.Errorf("duplicate grant: %v", err)
			}
			if _, err := s.AddGrant(ctx, DefaultWorkspace, Grant{Project: "shop", Role: "admin", Team: "nope"}); !errors.Is(err, ErrNotFound) {
				t.Errorf("unknown team grant: %v", err)
			}
			g2, err := s.AddGrant(ctx, DefaultWorkspace, Grant{Project: "shop", Role: "admin", Team: "platform"})
			if err != nil || g2.Team != "platform" {
				t.Fatalf("team grant: %+v %v", g2, err)
			}
			if _, err := s.AddGrant(ctx, DefaultWorkspace, Grant{Project: "blog", Role: "viewer", Team: "finance"}); err != nil {
				t.Fatal(err)
			}
			all, _ := s.ListGrants(ctx, DefaultWorkspace)
			shop, _ := s.ListProjectGrants(ctx, DefaultWorkspace, "shop")
			if len(all) != 3 || len(shop) != 2 || all[0].Project != "blog" {
				t.Errorf("grants: all=%d shop=%d first=%s", len(all), len(shop), all[0].Project)
			}

			// Deleting a team removes its grants; deleting a grant by id.
			if err := s.DeleteTeam(ctx, DefaultWorkspace, "finance"); err != nil {
				t.Fatal(err)
			}
			all, _ = s.ListGrants(ctx, DefaultWorkspace)
			if len(all) != 2 {
				t.Errorf("grants after team delete = %d", len(all))
			}
			if err := s.DeleteGrant(ctx, DefaultWorkspace, g1.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteGrant(ctx, DefaultWorkspace, g1.ID); !errors.Is(err, ErrNotFound) {
				t.Errorf("delete twice: %v", err)
			}
			if err := s.DeleteTeam(ctx, DefaultWorkspace, "nope"); !errors.Is(err, ErrNotFound) {
				t.Errorf("delete unknown team: %v", err)
			}
			if err := s.DeleteProjectGrants(ctx, DefaultWorkspace, "shop"); err != nil {
				t.Fatal(err)
			}
			if all, _ = s.ListGrants(ctx, DefaultWorkspace); len(all) != 0 {
				t.Errorf("grants after project delete = %d", len(all))
			}

			// Identities: first sight creates, later sights update.
			id1, err := s.TouchIdentity(ctx, DefaultWorkspace, Identity{Email: "Maria@Example.test", Name: "Maria", Provider: "google", Groups: []string{"finance"}})
			if err != nil || id1.Email != "maria@example.test" || id1.Realm != "workspace" || len(id1.Groups) != 1 {
				t.Fatalf("touch: %+v %v", id1, err)
			}
			time.Sleep(5 * time.Millisecond)
			id2, err := s.TouchIdentity(ctx, DefaultWorkspace, Identity{Email: "maria@example.test", Provider: "okta"})
			if err != nil || id2.ID != id1.ID || id2.Name != "Maria" || id2.Provider != "okta" || len(id2.Groups) != 1 || !id2.LastSeenAt.After(id1.FirstSeenAt) {
				t.Fatalf("touch again: %+v %v", id2, err)
			}
			ids, _ := s.ListIdentities(ctx, DefaultWorkspace)
			if len(ids) != 1 {
				t.Errorf("identities = %d", len(ids))
			}

			// Export / import round trip into a fresh store.
			if _, _, err := s.PutTeam(ctx, DefaultWorkspace, Team{Name: "finance", Members: []string{"fin@example.test"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddGrant(ctx, DefaultWorkspace, Grant{Project: "blog", Role: "user", Team: "finance"}); err != nil {
				t.Fatal(err)
			}
			dump, err := s.Export(ctx, DefaultWorkspace)
			if err != nil || dump.Version != DumpVersion || len(dump.Teams) != 2 || len(dump.Grants) != 1 || len(dump.Identities) != 1 {
				t.Fatalf("export: %+v %v", dump, err)
			}
			fresh := NewMemory()
			res, err := fresh.Import(ctx, DefaultWorkspace, dump, false)
			if err != nil || res.Teams != 2 || res.Grants != 1 || res.Identities != 1 {
				t.Fatalf("import: %+v %v", res, err)
			}
			res, err = fresh.Import(ctx, DefaultWorkspace, dump, false)
			if err != nil || res.Teams != 0 || res.Skipped != 3 {
				t.Errorf("import again without overwrite: %+v %v", res, err)
			}
			if err := s.DeleteIdentity(ctx, DefaultWorkspace, "maria@example.test"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

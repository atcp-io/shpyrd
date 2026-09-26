package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
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
			for _, stmt := range []string{"DROP TABLE IF EXISTS domain_claims", "DROP TABLE IF EXISTS edge_codes", "DROP TABLE IF EXISTS sessions", "DROP TABLE IF EXISTS grants", "DROP TABLE IF EXISTS teams", "DROP TABLE IF EXISTS identities", "DROP TABLE IF EXISTS workspaces", "DROP TABLE IF EXISTS schema_migrations"} {
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
			if w, err := s.UpdateWorkspaceSettings(ctx, DefaultWorkspace, WorkspaceSettings{JoinPolicy: JoinListed}); err != nil || w.Settings.JoinPolicy != JoinListed {
				t.Errorf("settings: %+v %v", w, err)
			}
			if w, _ := s.Workspace(ctx, DefaultWorkspace); w.Settings.JoinPolicy != JoinListed {
				t.Errorf("settings not persisted: %+v", w)
			}
			// Domain claims: created with a token, verified later, connector updatable.
			claim, err := s.PutDomainClaim(ctx, DefaultWorkspace, "Acme.com", "google")
			if err != nil || claim.Domain != "acme.com" || claim.Token == "" || claim.VerifiedAt != nil {
				t.Fatalf("claim: %+v %v", claim, err)
			}
			again, _ := s.PutDomainClaim(ctx, DefaultWorkspace, "acme.com", "okta")
			if again.Token != claim.Token || again.Connector != "okta" {
				t.Errorf("claim update: %+v", again)
			}
			if v, err := s.MarkDomainVerified(ctx, DefaultWorkspace, "acme.com", time.Now()); err != nil || v.VerifiedAt == nil {
				t.Errorf("verify: %+v %v", v, err)
			}
			if claims, _ := s.ListDomainClaims(ctx, DefaultWorkspace); len(claims) != 1 || claims[0].VerifiedAt == nil {
				t.Errorf("claims = %+v", claims)
			}
			if err := s.DeleteDomainClaim(ctx, DefaultWorkspace, "nope.com"); !errors.Is(err, ErrNotFound) {
				t.Errorf("delete unknown claim: %v", err)
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
			if err != nil || len(teams) != 3 || teams[0].Name != "everyone" || !teams[0].Everyone || teams[1].Name != "finance" {
				t.Fatalf("list teams: %+v %v", teams, err)
			}
			// The built-in team is neither editable nor deletable, but grantable.
			if _, _, err := s.PutTeam(ctx, DefaultWorkspace, Team{Name: TeamEveryone, Members: []string{"x@example.test"}}); !errors.Is(err, ErrBuiltIn) {
				t.Errorf("put everyone: %v", err)
			}
			if err := s.DeleteTeam(ctx, DefaultWorkspace, TeamEveryone); !errors.Is(err, ErrBuiltIn) {
				t.Errorf("delete everyone: %v", err)
			}
			if _, err := s.AddGrant(ctx, DefaultWorkspace, Grant{Project: "intranet", Role: "user", Team: TeamEveryone}); err != nil {
				t.Errorf("grant everyone: %v", err)
			}
			_ = s.DeleteProjectGrants(ctx, DefaultWorkspace, "intranet")
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
			if len(ids) != 1 || ids[0].Status != StatusActive {
				t.Errorf("identities = %+v", ids)
			}
			if sus, err := s.SetIdentityStatus(ctx, DefaultWorkspace, "maria@example.test", StatusSuspended); err != nil || sus.Status != StatusSuspended {
				t.Errorf("suspend: %v %+v", err, sus)
			}
			if _, err := s.SetIdentityStatus(ctx, DefaultWorkspace, "nobody@example.test", StatusSuspended); !errors.Is(err, ErrNotFound) {
				t.Errorf("suspend unknown: %v", err)
			}
			if _, err := s.SetIdentityStatus(ctx, DefaultWorkspace, "maria@example.test", StatusActive); err != nil {
				t.Fatal(err)
			}

			// Export / import round trip into a fresh store.
			if _, _, err := s.PutTeam(ctx, DefaultWorkspace, Team{Name: "finance", Members: []string{"fin@example.test"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddGrant(ctx, DefaultWorkspace, Grant{Project: "blog", Role: "user", Team: "finance"}); err != nil {
				t.Fatal(err)
			}
			dump, err := s.Export(ctx, DefaultWorkspace)
			if err != nil || dump.Version != DumpVersion || len(dump.Teams) != 3 || len(dump.Grants) != 1 || len(dump.Identities) != 1 || len(dump.Domains) != 1 {
				t.Fatalf("export: %+v %v", dump, err)
			}
			fresh := NewMemory()
			res, err := fresh.Import(ctx, DefaultWorkspace, dump, false)
			if err != nil || res.Teams != 2 || res.Grants != 1 || res.Identities != 1 {
				t.Fatalf("import: %+v %v", res, err)
			}
			if claims, _ := fresh.ListDomainClaims(ctx, DefaultWorkspace); len(claims) != 1 || claims[0].VerifiedAt == nil {
				t.Errorf("imported claims = %+v", claims)
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

func TestSessionsAndCodes(t *testing.T) {
	for name, open := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := open(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			sess := Session{ID: "sid-1", Identity: json.RawMessage(`{"email":"maria@acme.test"}`), CSRF: "c1", CreatedAt: now, LastSeenAt: now}
			if err := s.PutSession(ctx, DefaultWorkspace, sess); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetSession(ctx, "sid-1")
			if err != nil || got.CSRF != "c1" || got.WorkspaceID == "" || !strings.Contains(string(got.Identity), "maria") {
				t.Fatalf("get: %v %+v", err, got)
			}
			if err := s.TouchSession(ctx, "sid-1", now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			got, _ = s.GetSession(ctx, "sid-1")
			if !got.LastSeenAt.After(now) {
				t.Errorf("touch did not move last_seen: %v", got.LastSeenAt)
			}
			if err := s.TouchSession(ctx, "nope", now); !errors.Is(err, ErrNotFound) {
				t.Errorf("touch unknown: %v", err)
			}
			if n, _ := s.CountSessions(ctx, DefaultWorkspace); n != 1 {
				t.Errorf("count = %d", n)
			}
			// Purge by idle time and by age.
			_ = s.PutSession(ctx, DefaultWorkspace, Session{ID: "old", Identity: json.RawMessage(`{}`), CSRF: "c", CreatedAt: now.Add(-48 * time.Hour), LastSeenAt: now.Add(-2 * time.Hour)})
			n, err := s.PurgeSessions(ctx, now.Add(-24*time.Hour), now.Add(-12*time.Hour))
			if err != nil || n != 1 {
				t.Errorf("purge = %d %v", n, err)
			}
			if err := s.DeleteSession(ctx, "sid-1"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetSession(ctx, "sid-1"); !errors.Is(err, ErrNotFound) {
				t.Errorf("deleted session: %v", err)
			}

			// Codes: once, and not after expiry.
			if err := s.PutCode(ctx, Code{Code: "k1", Host: "app.acme.test", Claims: json.RawMessage(`{"sid":"sid-1"}`), ExpiresAt: now.Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			c, err := s.TakeCode(ctx, "k1")
			if err != nil || c.Host != "app.acme.test" || !strings.Contains(string(c.Claims), "sid-1") {
				t.Fatalf("take: %v %+v", err, c)
			}
			if _, err := s.TakeCode(ctx, "k1"); !errors.Is(err, ErrNotFound) {
				t.Errorf("second take: %v", err)
			}
			_ = s.PutCode(ctx, Code{Code: "k2", Host: "h", Claims: json.RawMessage(`{}`), ExpiresAt: now.Add(-time.Second)})
			if _, err := s.TakeCode(ctx, "k2"); !errors.Is(err, ErrNotFound) {
				t.Errorf("expired take: %v", err)
			}
		})
	}
}

package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Postgres is the Store on a PostgreSQL database: the in-cluster
// control-plane-db component, or a managed database (SHPYRD_DATABASE_URL).
type Postgres struct {
	pool *pgxpool.Pool
}

// Open connects; the URL is a libpq/pgx connection string.
func Open(ctx context.Context, url string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	cfg.MaxConns = 8
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

// Migrate applies the embedded migrations in order, each once, under an
// advisory lock so two servers starting together do not race.
func (p *Postgres) Migrate(ctx context.Context, defaultName string) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7245891)`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7245891)`) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	_, err = conn.Exec(ctx, `INSERT INTO workspaces (id, slug, name) VALUES ($1, $2, $3) ON CONFLICT (slug) DO NOTHING`, newID(), DefaultWorkspace, defaultName)
	return err
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (p *Postgres) wsID(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, slug string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id FROM workspaces WHERE slug = $1`, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

func (p *Postgres) Workspace(ctx context.Context, slug string) (*Workspace, error) {
	var w Workspace
	err := p.pool.QueryRow(ctx, `SELECT id, slug, name, created_at, updated_at FROM workspaces WHERE slug = $1`, slug).
		Scan(&w.ID, &w.Slug, &w.Name, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (p *Postgres) UpdateWorkspace(ctx context.Context, slug, name string) (*Workspace, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE workspaces SET name = $2, updated_at = now() WHERE slug = $1`, slug, name)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return p.Workspace(ctx, slug)
}

func (p *Postgres) TouchIdentity(ctx context.Context, ws string, id Identity) (*Identity, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	email := strings.ToLower(strings.TrimSpace(id.Email))
	realm := id.Realm
	if realm == "" {
		realm = "workspace"
	}
	groups, _ := json.Marshal(id.Groups)
	if id.Groups == nil {
		groups = nil // keep the stored value
	}
	var out Identity
	var groupsRaw []byte
	err = p.pool.QueryRow(ctx, `
		INSERT INTO identities (id, workspace_id, realm, email, name, provider, groups)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::jsonb, '[]'::jsonb))
		ON CONFLICT (workspace_id, realm, email) DO UPDATE SET
			name = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE identities.name END,
			provider = CASE WHEN EXCLUDED.provider <> '' THEN EXCLUDED.provider ELSE identities.provider END,
			groups = COALESCE($7::jsonb, identities.groups),
			last_seen_at = now()
		RETURNING id, workspace_id, realm, email, name, provider, groups, first_seen_at, last_seen_at`,
		newID(), wsID, realm, email, id.Name, id.Provider, groups).
		Scan(&out.ID, &out.WorkspaceID, &out.Realm, &out.Email, &out.Name, &out.Provider, &groupsRaw, &out.FirstSeenAt, &out.LastSeenAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(groupsRaw, &out.Groups)
	return &out, nil
}

func (p *Postgres) ListIdentities(ctx context.Context, ws string) ([]Identity, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `SELECT id, workspace_id, realm, email, name, provider, groups, first_seen_at, last_seen_at FROM identities WHERE workspace_id = $1 ORDER BY email`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var it Identity
		var groupsRaw []byte
		if err := rows.Scan(&it.ID, &it.WorkspaceID, &it.Realm, &it.Email, &it.Name, &it.Provider, &groupsRaw, &it.FirstSeenAt, &it.LastSeenAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(groupsRaw, &it.Groups)
		out = append(out, it)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteIdentity(ctx context.Context, ws, email string) error {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `DELETE FROM identities WHERE workspace_id = $1 AND email = $2`, wsID, strings.ToLower(email))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const teamColumns = `t.id, t.workspace_id, t.name, t.description, t.members, t.groups, t.platform_role, t.created_at, t.updated_at`

func scanTeam(row pgx.Row) (*Team, error) {
	var t Team
	var members, groups []byte
	if err := row.Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.Description, &members, &groups, &t.PlatformRole, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal(members, &t.Members)
	_ = json.Unmarshal(groups, &t.Groups)
	if t.Members == nil {
		t.Members = []string{}
	}
	if t.Groups == nil {
		t.Groups = []string{}
	}
	return &t, nil
}

func (p *Postgres) ListTeams(ctx context.Context, ws string) ([]Team, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `SELECT `+teamColumns+` FROM teams t WHERE t.workspace_id = $1 ORDER BY t.name`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Team
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (p *Postgres) GetTeam(ctx context.Context, ws, name string) (*Team, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	return scanTeam(p.pool.QueryRow(ctx, `SELECT `+teamColumns+` FROM teams t WHERE t.workspace_id = $1 AND t.name = $2`, wsID, name))
}

func (p *Postgres) PutTeam(ctx context.Context, ws string, t Team) (*Team, bool, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, false, err
	}
	members, _ := json.Marshal(normalizeEmails(t.Members))
	groups, _ := json.Marshal(dedupe(t.Groups))
	var inserted bool
	row := p.pool.QueryRow(ctx, `
		INSERT INTO teams (id, workspace_id, name, description, members, groups, platform_role)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (workspace_id, name) DO UPDATE SET
			description = EXCLUDED.description, members = EXCLUDED.members, groups = EXCLUDED.groups,
			platform_role = EXCLUDED.platform_role, updated_at = now()
		RETURNING `+strings.ReplaceAll(teamColumns, "t.", "")+`, (xmax = 0) AS inserted`,
		newID(), wsID, t.Name, t.Description, members, groups, t.PlatformRole)
	var out Team
	var m, g []byte
	if err := row.Scan(&out.ID, &out.WorkspaceID, &out.Name, &out.Description, &m, &g, &out.PlatformRole, &out.CreatedAt, &out.UpdatedAt, &inserted); err != nil {
		return nil, false, err
	}
	_ = json.Unmarshal(m, &out.Members)
	_ = json.Unmarshal(g, &out.Groups)
	return &out, inserted, nil
}

func (p *Postgres) DeleteTeam(ctx context.Context, ws, name string) error {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return err
	}
	// Grants of the team go with it (ON DELETE CASCADE).
	tag, err := p.pool.Exec(ctx, `DELETE FROM teams WHERE workspace_id = $1 AND name = $2`, wsID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const grantSelect = `SELECT g.id, g.workspace_id, g.project, g.role, g.user_email, COALESCE(t.name, ''), g.created_at
	FROM grants g LEFT JOIN teams t ON t.id = g.team_id`

func (p *Postgres) listGrants(ctx context.Context, where string, args ...any) ([]Grant, error) {
	rows, err := p.pool.Query(ctx, grantSelect+" WHERE "+where+" ORDER BY g.project, g.role, g.user_email, t.name", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.WorkspaceID, &g.Project, &g.Role, &g.User, &g.Team, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (p *Postgres) ListGrants(ctx context.Context, ws string) ([]Grant, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	return p.listGrants(ctx, "g.workspace_id = $1", wsID)
}

func (p *Postgres) ListProjectGrants(ctx context.Context, ws, project string) ([]Grant, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	return p.listGrants(ctx, "g.workspace_id = $1 AND g.project = $2", wsID, project)
}

func (p *Postgres) AddGrant(ctx context.Context, ws string, g Grant) (*Grant, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	var teamID *string
	if g.Team != "" {
		var id string
		err := p.pool.QueryRow(ctx, `SELECT id FROM teams WHERE workspace_id = $1 AND name = $2`, wsID, g.Team).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		teamID = &id
	}
	id := newID()
	_, err = p.pool.Exec(ctx, `INSERT INTO grants (id, workspace_id, project, role, user_email, team_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, wsID, g.Project, g.Role, strings.ToLower(strings.TrimSpace(g.User)), teamID)
	if err != nil {
		if isUnique(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	out, err := p.listGrants(ctx, "g.id = $1", id)
	if err != nil || len(out) == 0 {
		return nil, fmt.Errorf("grant %s: %v", id, err)
	}
	return &out[0], nil
}

func (p *Postgres) DeleteGrant(ctx context.Context, ws, id string) error {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `DELETE FROM grants WHERE workspace_id = $1 AND id = $2`, wsID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteProjectGrants(ctx context.Context, ws, project string) error {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `DELETE FROM grants WHERE workspace_id = $1 AND project = $2`, wsID, project)
	return err
}

func (p *Postgres) Export(ctx context.Context, ws string) (*Dump, error) {
	w, err := p.Workspace(ctx, ws)
	if err != nil {
		return nil, err
	}
	ids, err := p.ListIdentities(ctx, ws)
	if err != nil {
		return nil, err
	}
	teams, err := p.ListTeams(ctx, ws)
	if err != nil {
		return nil, err
	}
	grants, err := p.ListGrants(ctx, ws)
	if err != nil {
		return nil, err
	}
	return &Dump{Version: DumpVersion, Workspace: *w, Identities: ids, Teams: teams, Grants: grants}, nil
}

func (p *Postgres) Import(ctx context.Context, ws string, d *Dump, overwrite bool) (*ImportResult, error) {
	return importDump(ctx, p, ws, d, overwrite)
}

var _ Store = (*Postgres)(nil)
var _ Store = (*Memory)(nil)

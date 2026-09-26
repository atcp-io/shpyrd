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
	if _, err := conn.Exec(ctx, `INSERT INTO workspaces (id, slug, name) VALUES ($1, $2, $3) ON CONFLICT (slug) DO NOTHING`, newID(), DefaultWorkspace, defaultName); err != nil {
		return err
	}
	// The built-in team of the implicit workspace.
	_, err = conn.Exec(ctx, `INSERT INTO teams (id, workspace_id, name, description, kind)
		SELECT $1, id, $2, 'Everyone who has signed in', 'everyone' FROM workspaces WHERE slug = $3
		ON CONFLICT (workspace_id, name) DO UPDATE SET kind = 'everyone'`, newID(), TeamEveryone, DefaultWorkspace)
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
	var settings []byte
	err := p.pool.QueryRow(ctx, `SELECT id, slug, name, settings, created_at, updated_at FROM workspaces WHERE slug = $1`, slug).
		Scan(&w.ID, &w.Slug, &w.Name, &settings, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(settings, &w.Settings)
	return &w, nil
}

func (p *Postgres) UpdateWorkspaceSettings(ctx context.Context, slug string, settings WorkspaceSettings) (*Workspace, error) {
	raw, _ := json.Marshal(settings)
	tag, err := p.pool.Exec(ctx, `UPDATE workspaces SET settings = $2, updated_at = now() WHERE slug = $1`, slug, raw)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return p.Workspace(ctx, slug)
}

const claimColumns = `id, workspace_id, domain, token, connector, verified_at, created_at`

func scanClaim(row pgx.Row) (*DomainClaim, error) {
	var d DomainClaim
	if err := row.Scan(&d.ID, &d.WorkspaceID, &d.Domain, &d.Token, &d.Connector, &d.VerifiedAt, &d.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &d, nil
}

func (p *Postgres) ListDomainClaims(ctx context.Context, ws string) ([]DomainClaim, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `SELECT `+claimColumns+` FROM domain_claims WHERE workspace_id = $1 ORDER BY domain`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DomainClaim
	for rows.Next() {
		d, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func (p *Postgres) PutDomainClaim(ctx context.Context, ws, domain, connector string) (*DomainClaim, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	return scanClaim(p.pool.QueryRow(ctx, `INSERT INTO domain_claims (id, workspace_id, domain, token, connector) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workspace_id, domain) DO UPDATE SET connector = EXCLUDED.connector
		RETURNING `+claimColumns, newID(), wsID, domain, newID(), connector))
}

func (p *Postgres) MarkDomainVerified(ctx context.Context, ws, domain string, at time.Time) (*DomainClaim, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	return scanClaim(p.pool.QueryRow(ctx, `UPDATE domain_claims SET verified_at = $3 WHERE workspace_id = $1 AND domain = $2 RETURNING `+claimColumns, wsID, strings.ToLower(domain), at))
}

func (p *Postgres) DeleteDomainClaim(ctx context.Context, ws, domain string) error {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `DELETE FROM domain_claims WHERE workspace_id = $1 AND domain = $2`, wsID, strings.ToLower(domain))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
		RETURNING id, workspace_id, realm, email, name, provider, groups, status, first_seen_at, last_seen_at`,
		newID(), wsID, realm, email, id.Name, id.Provider, groups).
		Scan(&out.ID, &out.WorkspaceID, &out.Realm, &out.Email, &out.Name, &out.Provider, &groupsRaw, &out.Status, &out.FirstSeenAt, &out.LastSeenAt)
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
	rows, err := p.pool.Query(ctx, `SELECT id, workspace_id, realm, email, name, provider, groups, status, first_seen_at, last_seen_at FROM identities WHERE workspace_id = $1 ORDER BY email`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var it Identity
		var groupsRaw []byte
		if err := rows.Scan(&it.ID, &it.WorkspaceID, &it.Realm, &it.Email, &it.Name, &it.Provider, &groupsRaw, &it.Status, &it.FirstSeenAt, &it.LastSeenAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(groupsRaw, &it.Groups)
		out = append(out, it)
	}
	return out, rows.Err()
}

func (p *Postgres) GetIdentity(ctx context.Context, ws, email string) (*Identity, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	var it Identity
	var groupsRaw []byte
	err = p.pool.QueryRow(ctx, `SELECT id, workspace_id, realm, email, name, provider, groups, status, first_seen_at, last_seen_at FROM identities WHERE workspace_id = $1 AND lower(email) = lower($2)`, wsID, email).
		Scan(&it.ID, &it.WorkspaceID, &it.Realm, &it.Email, &it.Name, &it.Provider, &groupsRaw, &it.Status, &it.FirstSeenAt, &it.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(groupsRaw, &it.Groups)
	return &it, nil
}

func (p *Postgres) SetIdentityStatus(ctx context.Context, ws, email, status string) (*Identity, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	var out Identity
	var groupsRaw []byte
	err = p.pool.QueryRow(ctx, `UPDATE identities SET status = $3 WHERE workspace_id = $1 AND email = $2
		RETURNING id, workspace_id, realm, email, name, provider, groups, status, first_seen_at, last_seen_at`, wsID, strings.ToLower(email), status).
		Scan(&out.ID, &out.WorkspaceID, &out.Realm, &out.Email, &out.Name, &out.Provider, &groupsRaw, &out.Status, &out.FirstSeenAt, &out.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(groupsRaw, &out.Groups)
	return &out, nil
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

const teamColumns = `t.id, t.workspace_id, t.name, t.description, t.members, t.groups, t.platform_role, t.kind, t.created_at, t.updated_at`

func scanTeam(row pgx.Row) (*Team, error) {
	var t Team
	var members, groups []byte
	var kind string
	if err := row.Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.Description, &members, &groups, &t.PlatformRole, &kind, &t.CreatedAt, &t.UpdatedAt); err != nil {
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
	t.Everyone = kind == "everyone"
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
	if t.Name == TeamEveryone {
		return nil, false, ErrBuiltIn
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
	var kind string
	if err := row.Scan(&out.ID, &out.WorkspaceID, &out.Name, &out.Description, &m, &g, &out.PlatformRole, &kind, &out.CreatedAt, &out.UpdatedAt, &inserted); err != nil {
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
	if name == TeamEveryone {
		return ErrBuiltIn
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
	domains, err := p.ListDomainClaims(ctx, ws)
	if err != nil {
		return nil, err
	}
	return &Dump{Version: DumpVersion, Workspace: *w, Identities: ids, Teams: teams, Grants: grants, Domains: domains}, nil
}

func (p *Postgres) Import(ctx context.Context, ws string, d *Dump, overwrite bool) (*ImportResult, error) {
	return importDump(ctx, p, ws, d, overwrite)
}

// ---- sessions and codes ------------------------------------------------------

func (p *Postgres) PutSession(ctx context.Context, ws string, sess Session) error {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return err
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now()
	}
	if sess.LastSeenAt.IsZero() {
		sess.LastSeenAt = sess.CreatedAt
	}
	identity := sess.Identity
	if len(identity) == 0 {
		identity = json.RawMessage("{}")
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO sessions (id, workspace_id, identity, csrf, id_token, created_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET identity = EXCLUDED.identity, csrf = EXCLUDED.csrf, id_token = EXCLUDED.id_token, last_seen_at = EXCLUDED.last_seen_at`,
		sess.ID, wsID, identity, sess.CSRF, sess.IDToken, sess.CreatedAt, sess.LastSeenAt)
	return err
}

func (p *Postgres) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	err := p.pool.QueryRow(ctx, `SELECT id, workspace_id, identity, csrf, id_token, created_at, last_seen_at FROM sessions WHERE id = $1`, id).
		Scan(&s.ID, &s.WorkspaceID, &s.Identity, &s.CSRF, &s.IDToken, &s.CreatedAt, &s.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (p *Postgres) TouchSession(ctx context.Context, id string, at time.Time) error {
	tag, err := p.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id = $1 AND last_seen_at < $2`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

func (p *Postgres) DeleteSession(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

func (p *Postgres) PurgeSessions(ctx context.Context, createdBefore, seenBefore time.Time) (int, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE created_at < $1 OR last_seen_at < $2`, createdBefore, seenBefore)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (p *Postgres) CountSessions(ctx context.Context, ws string) (int, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return 0, err
	}
	var n int
	err = p.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE workspace_id = $1`, wsID).Scan(&n)
	return n, err
}

func (p *Postgres) PutCode(ctx context.Context, c Code) error {
	if _, err := p.pool.Exec(ctx, `DELETE FROM edge_codes WHERE expires_at < now()`); err != nil {
		return err
	}
	claims := c.Claims
	if len(claims) == 0 {
		claims = json.RawMessage("{}")
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO edge_codes (code, host, claims, expires_at) VALUES ($1, $2, $3, $4)`, c.Code, c.Host, claims, c.ExpiresAt)
	return err
}

func (p *Postgres) TakeCode(ctx context.Context, code string) (*Code, error) {
	var c Code
	err := p.pool.QueryRow(ctx, `DELETE FROM edge_codes WHERE code = $1 RETURNING code, host, claims, expires_at`, code).
		Scan(&c.Code, &c.Host, &c.Claims, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if time.Now().After(c.ExpiresAt) {
		return nil, ErrNotFound
	}
	return &c, nil
}

// ---- API tokens (RFC-0031) -------------------------------------------------

func (p *Postgres) CreateToken(ctx context.Context, ws string, t APIToken, hash string) (*APIToken, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	roles, _ := json.Marshal(t.ProjectRoles)
	row := p.pool.QueryRow(ctx, `INSERT INTO api_tokens (id, workspace_id, name, owner_email, hash, platform_role, project_roles, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, workspace_id, name, owner_email, platform_role, project_roles, created_at, expires_at, last_used_at`,
		newID(), wsID, t.Name, strings.ToLower(t.OwnerEmail), hash, t.PlatformRole, roles, t.ExpiresAt)
	out, err := scanToken(row)
	if isUnique(err) {
		return nil, ErrConflict
	}
	return out, err
}

func scanToken(row pgx.Row) (*APIToken, error) {
	var t APIToken
	var roles []byte
	if err := row.Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.OwnerEmail, &t.PlatformRole, &roles, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	_ = json.Unmarshal(roles, &t.ProjectRoles)
	return &t, nil
}

func (p *Postgres) LookupToken(ctx context.Context, hash string) (*APIToken, error) {
	row := p.pool.QueryRow(ctx, `SELECT id, workspace_id, name, owner_email, platform_role, project_roles, created_at, expires_at, last_used_at
		FROM api_tokens WHERE hash = $1 AND (expires_at IS NULL OR expires_at > now())`, hash)
	t, err := scanToken(row)
	if err != nil || t == nil {
		return t, err
	}
	// Update last_used_at at most once a minute (best effort).
	if t.LastUsedAt == nil || time.Since(*t.LastUsedAt) > time.Minute {
		now := time.Now()
		_, _ = p.pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = $2::timestamptz WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '1 minute')`, t.ID, now)
		t.LastUsedAt = &now
	}
	return t, nil
}

func (p *Postgres) ListTokens(ctx context.Context, ws, ownerEmail string) ([]APIToken, error) {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return nil, err
	}
	q := `SELECT id, workspace_id, name, owner_email, platform_role, project_roles, created_at, expires_at, last_used_at FROM api_tokens WHERE workspace_id = $1`
	args := []any{wsID}
	if ownerEmail != "" {
		q += ` AND lower(owner_email) = lower($2)`
		args = append(args, ownerEmail)
	}
	q += ` ORDER BY name`
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		if t != nil {
			out = append(out, *t)
		}
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteToken(ctx context.Context, ws, id string) error {
	wsID, err := p.wsID(ctx, p.pool, ws)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `DELETE FROM api_tokens WHERE workspace_id = $1 AND id = $2`, wsID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

var _ Store = (*Postgres)(nil)
var _ Store = (*Memory)(nil)

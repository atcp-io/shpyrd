package store

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Memory is the in-memory Store: tests, and the reference for the SQL one.
type Memory struct {
	mu         sync.Mutex
	workspaces map[string]*Workspace // by slug
	identities []Identity
	teams      []Team
	grants     []Grant
	now        func() time.Time
}

// NewMemory returns an empty store with the implicit workspace.
func NewMemory() *Memory {
	m := &Memory{workspaces: map[string]*Workspace{}, now: func() time.Time { return time.Now().UTC() }}
	_ = m.Migrate(context.Background(), "shpyrd")
	return m
}

func newID() string { return uuid.NewString() }

func (m *Memory) Migrate(_ context.Context, defaultName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.workspaces[DefaultWorkspace]; !ok {
		t := m.now()
		m.workspaces[DefaultWorkspace] = &Workspace{ID: newID(), Slug: DefaultWorkspace, Name: defaultName, CreatedAt: t, UpdatedAt: t}
	}
	return nil
}

func (m *Memory) Close() {}

func (m *Memory) ws(slug string) (*Workspace, error) {
	w, ok := m.workspaces[slug]
	if !ok {
		return nil, ErrNotFound
	}
	return w, nil
}

func (m *Memory) Workspace(_ context.Context, slug string) (*Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(slug)
	if err != nil {
		return nil, err
	}
	c := *w
	return &c, nil
}

func (m *Memory) UpdateWorkspace(_ context.Context, slug, name string) (*Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(slug)
	if err != nil {
		return nil, err
	}
	w.Name, w.UpdatedAt = name, m.now()
	c := *w
	return &c, nil
}

func (m *Memory) TouchIdentity(_ context.Context, ws string, id Identity) (*Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	email := strings.ToLower(strings.TrimSpace(id.Email))
	realm := id.Realm
	if realm == "" {
		realm = "workspace"
	}
	now := m.now()
	for i := range m.identities {
		it := &m.identities[i]
		if it.WorkspaceID == w.ID && it.Realm == realm && it.Email == email {
			if id.Name != "" {
				it.Name = id.Name
			}
			if id.Provider != "" {
				it.Provider = id.Provider
			}
			if id.Groups != nil {
				it.Groups = append([]string(nil), id.Groups...)
			}
			it.LastSeenAt = now
			c := *it
			return &c, nil
		}
	}
	it := Identity{ID: newID(), WorkspaceID: w.ID, Realm: realm, Email: email, Name: id.Name, Provider: id.Provider, Groups: append([]string(nil), id.Groups...), FirstSeenAt: now, LastSeenAt: now}
	m.identities = append(m.identities, it)
	return &it, nil
}

func (m *Memory) ListIdentities(_ context.Context, ws string) ([]Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	var out []Identity
	for _, it := range m.identities {
		if it.WorkspaceID == w.ID {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

func (m *Memory) DeleteIdentity(_ context.Context, ws, email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return err
	}
	email = strings.ToLower(email)
	for i, it := range m.identities {
		if it.WorkspaceID == w.ID && it.Email == email {
			m.identities = append(m.identities[:i], m.identities[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) ListTeams(_ context.Context, ws string) ([]Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	var out []Team
	for _, t := range m.teams {
		if t.WorkspaceID == w.ID {
			out = append(out, cloneTeam(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) GetTeam(_ context.Context, ws, name string) (*Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	for _, t := range m.teams {
		if t.WorkspaceID == w.ID && t.Name == name {
			c := cloneTeam(t)
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) PutTeam(_ context.Context, ws string, t Team) (*Team, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, false, err
	}
	t.Members = normalizeEmails(t.Members)
	t.Groups = dedupe(t.Groups)
	now := m.now()
	for i := range m.teams {
		e := &m.teams[i]
		if e.WorkspaceID == w.ID && e.Name == t.Name {
			e.Description, e.Members, e.Groups, e.PlatformRole, e.UpdatedAt = t.Description, t.Members, t.Groups, t.PlatformRole, now
			c := cloneTeam(*e)
			return &c, false, nil
		}
	}
	t.ID, t.WorkspaceID, t.CreatedAt, t.UpdatedAt = newID(), w.ID, now, now
	m.teams = append(m.teams, t)
	c := cloneTeam(t)
	return &c, true, nil
}

func (m *Memory) DeleteTeam(_ context.Context, ws, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return err
	}
	for i, t := range m.teams {
		if t.WorkspaceID == w.ID && t.Name == name {
			m.teams = append(m.teams[:i], m.teams[i+1:]...)
			kept := m.grants[:0]
			for _, g := range m.grants {
				if !(g.WorkspaceID == w.ID && g.Team == name) {
					kept = append(kept, g)
				}
			}
			m.grants = kept
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) ListGrants(_ context.Context, ws string) ([]Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	var out []Grant
	for _, g := range m.grants {
		if g.WorkspaceID == w.ID {
			out = append(out, g)
		}
	}
	sortGrants(out)
	return out, nil
}

func (m *Memory) ListProjectGrants(ctx context.Context, ws, project string) ([]Grant, error) {
	all, err := m.ListGrants(ctx, ws)
	if err != nil {
		return nil, err
	}
	var out []Grant
	for _, g := range all {
		if g.Project == project {
			out = append(out, g)
		}
	}
	return out, nil
}

func (m *Memory) AddGrant(_ context.Context, ws string, g Grant) (*Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	g.User = strings.ToLower(strings.TrimSpace(g.User))
	if g.Team != "" {
		found := false
		for _, t := range m.teams {
			if t.WorkspaceID == w.ID && t.Name == g.Team {
				found = true
			}
		}
		if !found {
			return nil, ErrNotFound
		}
	}
	for _, e := range m.grants {
		if e.WorkspaceID == w.ID && e.Project == g.Project && e.Role == g.Role && e.User == g.User && e.Team == g.Team {
			return nil, ErrConflict
		}
	}
	g.ID, g.WorkspaceID, g.CreatedAt = newID(), w.ID, m.now()
	m.grants = append(m.grants, g)
	c := g
	return &c, nil
}

func (m *Memory) DeleteGrant(_ context.Context, ws, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return err
	}
	for i, g := range m.grants {
		if g.WorkspaceID == w.ID && g.ID == id {
			m.grants = append(m.grants[:i], m.grants[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) DeleteProjectGrants(_ context.Context, ws, project string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return err
	}
	kept := m.grants[:0]
	for _, g := range m.grants {
		if !(g.WorkspaceID == w.ID && g.Project == project) {
			kept = append(kept, g)
		}
	}
	m.grants = kept
	return nil
}

func (m *Memory) Export(ctx context.Context, ws string) (*Dump, error) {
	w, err := m.Workspace(ctx, ws)
	if err != nil {
		return nil, err
	}
	ids, _ := m.ListIdentities(ctx, ws)
	teams, _ := m.ListTeams(ctx, ws)
	grants, _ := m.ListGrants(ctx, ws)
	return &Dump{Version: DumpVersion, Workspace: *w, Identities: ids, Teams: teams, Grants: grants}, nil
}

func (m *Memory) Import(ctx context.Context, ws string, d *Dump, overwrite bool) (*ImportResult, error) {
	return importDump(ctx, m, ws, d, overwrite)
}

// importDump is the Import every implementation shares: it goes through the
// public methods so the rules (normalisation, team existence, conflicts)
// are the same everywhere.
func importDump(ctx context.Context, s Store, ws string, d *Dump, overwrite bool) (*ImportResult, error) {
	res := &ImportResult{}
	if d.Workspace.Name != "" && overwrite {
		if _, err := s.UpdateWorkspace(ctx, ws, d.Workspace.Name); err != nil {
			return res, err
		}
	}
	for _, t := range d.Teams {
		if !overwrite {
			if _, err := s.GetTeam(ctx, ws, t.Name); err == nil {
				res.Skipped++
				continue
			}
		}
		if _, _, err := s.PutTeam(ctx, ws, t); err != nil {
			return res, err
		}
		res.Teams++
	}
	for _, g := range d.Grants {
		_, err := s.AddGrant(ctx, ws, g)
		switch {
		case err == nil:
			res.Grants++
		case err == ErrConflict:
			res.Skipped++
		default:
			return res, err
		}
	}
	for _, id := range d.Identities {
		if _, err := s.TouchIdentity(ctx, ws, id); err != nil {
			return res, err
		}
		res.Identities++
	}
	return res, nil
}

func cloneTeam(t Team) Team {
	t.Members = append([]string(nil), t.Members...)
	t.Groups = append([]string(nil), t.Groups...)
	return t
}

func sortGrants(gs []Grant) {
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].Project != gs[j].Project {
			return gs[i].Project < gs[j].Project
		}
		if gs[i].Role != gs[j].Role {
			return gs[i].Role < gs[j].Role
		}
		return gs[i].User+gs[i].Team < gs[j].User+gs[j].Team
	})
}

// normalizeEmails lower-cases, trims and dedupes, keeping order.
func normalizeEmails(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

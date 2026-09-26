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
type tokenEntry struct {
	token       APIToken
	hash        string
	lastUpdated time.Time
}

type Memory struct {
	mu         sync.Mutex
	workspaces map[string]*Workspace // by slug
	identities []Identity
	teams      []Team
	grants     []Grant
	sessions   map[string]*Session
	codes      map[string]*Code
	domains    []DomainClaim
	tokens     []tokenEntry
	now        func() time.Time
}

// NewMemory returns an empty store with the implicit workspace.
func NewMemory() *Memory {
	m := &Memory{workspaces: map[string]*Workspace{}, sessions: map[string]*Session{}, codes: map[string]*Code{}, now: func() time.Time { return time.Now().UTC() }}
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
	w := m.workspaces[DefaultWorkspace]
	found := false
	for _, t := range m.teams {
		if t.WorkspaceID == w.ID && t.Everyone {
			found = true
		}
	}
	if !found {
		t := m.now()
		m.teams = append(m.teams, Team{ID: newID(), WorkspaceID: w.ID, Name: TeamEveryone, Description: "Everyone who has signed in", Members: []string{}, Groups: []string{}, Everyone: true, CreatedAt: t, UpdatedAt: t})
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

func (m *Memory) UpdateWorkspaceSettings(_ context.Context, slug string, settings WorkspaceSettings) (*Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(slug)
	if err != nil {
		return nil, err
	}
	w.Settings, w.UpdatedAt = settings, m.now()
	c := *w
	return &c, nil
}

func (m *Memory) ListDomainClaims(_ context.Context, ws string) ([]DomainClaim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	var out []DomainClaim
	for _, d := range m.domains {
		if d.WorkspaceID == w.ID {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, nil
}

func (m *Memory) PutDomainClaim(_ context.Context, ws, domain, connector string) (*DomainClaim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	for i := range m.domains {
		d := &m.domains[i]
		if d.WorkspaceID == w.ID && d.Domain == domain {
			d.Connector = connector
			c := *d
			return &c, nil
		}
	}
	d := DomainClaim{ID: newID(), WorkspaceID: w.ID, Domain: domain, Token: newID(), Connector: connector, CreatedAt: m.now()}
	m.domains = append(m.domains, d)
	return &d, nil
}

func (m *Memory) MarkDomainVerified(_ context.Context, ws, domain string, at time.Time) (*DomainClaim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	for i := range m.domains {
		d := &m.domains[i]
		if d.WorkspaceID == w.ID && d.Domain == strings.ToLower(domain) {
			t := at
			d.VerifiedAt = &t
			c := *d
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) DeleteDomainClaim(_ context.Context, ws, domain string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return err
	}
	for i, d := range m.domains {
		if d.WorkspaceID == w.ID && d.Domain == strings.ToLower(domain) {
			m.domains = append(m.domains[:i], m.domains[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
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
	it := Identity{ID: newID(), WorkspaceID: w.ID, Realm: realm, Email: email, Name: id.Name, Provider: id.Provider, Groups: append([]string(nil), id.Groups...), Status: StatusActive, FirstSeenAt: now, LastSeenAt: now}
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

func (m *Memory) GetIdentity(_ context.Context, ws, email string) (*Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	for _, it := range m.identities {
		if it.WorkspaceID == w.ID && strings.EqualFold(it.Email, email) {
			c := it
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) SetIdentityStatus(_ context.Context, ws, email, status string) (*Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	email = strings.ToLower(email)
	for i := range m.identities {
		it := &m.identities[i]
		if it.WorkspaceID == w.ID && it.Email == email {
			it.Status = status
			c := *it
			return &c, nil
		}
	}
	return nil, ErrNotFound
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
	t.Everyone = false
	now := m.now()
	for i := range m.teams {
		e := &m.teams[i]
		if e.WorkspaceID == w.ID && e.Name == t.Name {
			if e.Everyone {
				return nil, false, ErrBuiltIn
			}
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
			if t.Everyone {
				return ErrBuiltIn
			}
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
	domains, _ := m.ListDomainClaims(ctx, ws)
	return &Dump{Version: DumpVersion, Workspace: *w, Identities: ids, Teams: teams, Grants: grants, Domains: domains}, nil
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
		if _, err := s.UpdateWorkspaceSettings(ctx, ws, d.Workspace.Settings); err != nil {
			return res, err
		}
	}
	for _, dc := range d.Domains {
		claim, err := s.PutDomainClaim(ctx, ws, dc.Domain, dc.Connector)
		if err != nil {
			return res, err
		}
		if dc.VerifiedAt != nil && claim.VerifiedAt == nil {
			_, _ = s.MarkDomainVerified(ctx, ws, dc.Domain, *dc.VerifiedAt)
		}
	}
	for _, t := range d.Teams {
		if t.Everyone || t.Name == TeamEveryone {
			continue // built in everywhere
		}
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
		if id.Status == StatusSuspended {
			_, _ = s.SetIdentityStatus(ctx, ws, id.Email, StatusSuspended)
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

func (m *Memory) PutSession(_ context.Context, ws string, sess Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return err
	}
	sess.WorkspaceID = w.ID
	c := sess
	m.sessions[sess.ID] = &c
	return nil
}

func (m *Memory) GetSession(_ context.Context, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := *s
	return &c, nil
}

func (m *Memory) TouchSession(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return ErrNotFound
	}
	s.LastSeenAt = at
	return nil
}

func (m *Memory) DeleteSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

func (m *Memory) PurgeSessions(_ context.Context, createdBefore, seenBefore time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, s := range m.sessions {
		if s.CreatedAt.Before(createdBefore) || s.LastSeenAt.Before(seenBefore) {
			delete(m.sessions, id)
			n++
		}
	}
	return n, nil
}

func (m *Memory) CountSessions(_ context.Context, ws string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range m.sessions {
		if s.WorkspaceID == w.ID {
			n++
		}
	}
	return n, nil
}

func (m *Memory) PutCode(_ context.Context, c Code) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for k, e := range m.codes {
		if now.After(e.ExpiresAt) {
			delete(m.codes, k)
		}
	}
	cc := c
	m.codes[c.Code] = &cc
	return nil
}

func (m *Memory) TakeCode(_ context.Context, code string) (*Code, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.codes[code]
	if !ok {
		return nil, ErrNotFound
	}
	delete(m.codes, code)
	if m.now().After(c.ExpiresAt) {
		return nil, ErrNotFound
	}
	cc := *c
	return &cc, nil
}

// ---- API tokens (RFC-0031) -------------------------------------------------

func (m *Memory) CreateToken(_ context.Context, ws string, t APIToken, hash string) (*APIToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	t.OwnerEmail = strings.ToLower(t.OwnerEmail)
	for _, e := range m.tokens {
		if e.token.WorkspaceID == w.ID && e.token.OwnerEmail == t.OwnerEmail && e.token.Name == t.Name {
			return nil, ErrConflict
		}
	}
	t.ID, t.WorkspaceID = newID(), w.ID
	t.CreatedAt = m.now()
	c := t
	m.tokens = append(m.tokens, tokenEntry{token: c, hash: hash})
	return &c, nil
}

func (m *Memory) LookupToken(_ context.Context, hash string) (*APIToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for i := range m.tokens {
		e := &m.tokens[i]
		if e.hash != hash {
			continue
		}
		if e.token.ExpiresAt != nil && now.After(*e.token.ExpiresAt) {
			return nil, nil
		}
		if now.Sub(e.lastUpdated) > time.Minute {
			e.token.LastUsedAt = &now
			e.lastUpdated = now
		}
		c := e.token
		return &c, nil
	}
	return nil, nil
}

func (m *Memory) ListTokens(_ context.Context, ws, ownerEmail string) ([]APIToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return nil, err
	}
	var out []APIToken
	for _, e := range m.tokens {
		if e.token.WorkspaceID != w.ID {
			continue
		}
		if ownerEmail != "" && !strings.EqualFold(e.token.OwnerEmail, ownerEmail) {
			continue
		}
		out = append(out, e.token)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) DeleteToken(_ context.Context, ws, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.ws(ws)
	if err != nil {
		return err
	}
	for i, e := range m.tokens {
		if e.token.WorkspaceID == w.ID && e.token.ID == id {
			m.tokens = append(m.tokens[:i], m.tokens[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

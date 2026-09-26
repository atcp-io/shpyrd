package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/store"
)

// The workspace (RFC-0033): the tenant every project belongs to. The
// open-source platform has exactly one, implicit; the API exposes its name
// and the people it has seen sign in, so a "Workspace" page exists before
// the multi-workspace cloud does.

// WorkspaceView is GET /api/workspace.
type WorkspaceView struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Implicit bool   `json:"implicit"`
	// Domain is where the workspace's apps live, one label under it.
	Domain string `json:"domain,omitempty"`
	// Address is the host of an explicit workspace's dashboard (RFC-0033
	// phase 6); empty for the implicit workspace, which answers at the
	// platform's dashboard URL.
	Address string `json:"address,omitempty"`
	// URL is where this workspace's dashboard answers.
	URL    string `json:"url"`
	Status string `json:"status"`
	// Limits is the workspace's plan (nil: no ceiling) and Usage what it
	// uses today, in the plan's terms.
	Limits *store.Limits `json:"limits,omitempty"`
	Usage  *Usage        `json:"usage,omitempty"`
	// JoinPolicy says who becomes a person on first sign-in: open,
	// company (through a claimed domain's method) or listed (already named
	// in a team or a grant).
	JoinPolicy string    `json:"joinPolicy"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// PersonView is one identity the workspace has seen.
type PersonView struct {
	Email       string    `json:"email"`
	Name        string    `json:"name,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Groups      []string  `json:"groups"`
	Realm       string    `json:"realm"`
	Status      string    `json:"status"` // active, suspended
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
}

func personView(id store.Identity) PersonView {
	groups := id.Groups
	if groups == nil {
		groups = []string{}
	}
	return PersonView{Email: id.Email, Name: id.Name, Provider: id.Provider, Groups: groups, Realm: id.Realm, Status: firstNonEmpty(id.Status, store.StatusActive), FirstSeenAt: id.FirstSeenAt, LastSeenAt: id.LastSeenAt}
}

func (s *Server) workspaceView(w *store.Workspace) WorkspaceView {
	return WorkspaceView{
		Slug: w.Slug, Name: w.Name, Implicit: w.Implicit(),
		Domain: s.appsDomainOf(w), Address: w.Address, URL: s.dashboardURLOf(w), Status: firstNonEmpty(w.Status, store.WorkspaceActive),
		JoinPolicy: firstNonEmpty(w.Settings.JoinPolicy, store.JoinOpen), CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
}

func (s *Server) getWorkspace(c *gin.Context) {
	w, err := s.store.Workspace(c.Request.Context(), s.workspace(c))
	if err != nil {
		storeErr(c, err, "workspace")
		return
	}
	view := s.workspaceView(w)
	if w.Settings.Limits != nil {
		view.Limits = w.Settings.Limits
		view.Usage = s.usageOf(c.Request.Context(), w.Slug)
	}
	c.JSON(http.StatusOK, view)
}

func (s *Server) updateWorkspace(c *gin.Context) {
	var req struct {
		Name       *string `json:"name"`
		JoinPolicy *string `json:"joinPolicy"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	ctx := c.Request.Context()
	w, err := s.store.Workspace(ctx, s.workspace(c))
	if err != nil {
		storeErr(c, err, "workspace")
		return
	}
	var changes []string
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 80 {
			abort(c, http.StatusBadRequest, errors.New("name must be 1 to 80 characters"))
			return
		}
		if w, err = s.store.UpdateWorkspace(ctx, s.workspace(c), name); err != nil {
			storeErr(c, err, "workspace")
			return
		}
		changes = append(changes, "name: "+name)
	}
	if req.JoinPolicy != nil {
		switch *req.JoinPolicy {
		case store.JoinOpen, store.JoinCompany, store.JoinListed:
		default:
			abort(c, http.StatusBadRequest, errors.New("joinPolicy must be open, company or listed"))
			return
		}
		settings := w.Settings
		settings.JoinPolicy = *req.JoinPolicy
		if w, err = s.store.UpdateWorkspaceSettings(ctx, s.workspace(c), settings); err != nil {
			storeErr(c, err, "workspace")
			return
		}
		changes = append(changes, "join policy: "+*req.JoinPolicy)
	}
	if len(changes) > 0 {
		s.audit(c, "", "workspace.update", w.Slug, strings.Join(changes, ", "))
	}
	c.JSON(http.StatusOK, s.workspaceView(w))
}

// ---- domain claims -----------------------------------------------------------

// DomainClaimView is one claimed email domain.
type DomainClaimView struct {
	Domain     string     `json:"domain"`
	Connector  string     `json:"connector,omitempty"`
	Verified   bool       `json:"verified"`
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
	// Record is the DNS TXT record that proves ownership.
	Record      string `json:"record"`
	RecordValue string `json:"recordValue"`
}

func claimView(d store.DomainClaim) DomainClaimView {
	return DomainClaimView{Domain: d.Domain, Connector: d.Connector, Verified: d.VerifiedAt != nil, VerifiedAt: d.VerifiedAt, Record: "_shpyrd-verify." + d.Domain, RecordValue: "shpyrd-verify=" + d.Token}
}

func (s *Server) listDomainClaims(c *gin.Context) {
	claims, err := s.store.ListDomainClaims(c.Request.Context(), s.workspace(c))
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]DomainClaimView, 0, len(claims))
	for _, d := range claims {
		out = append(out, claimView(d))
	}
	c.JSON(http.StatusOK, out)
}

var emailDomainRe = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)

func (s *Server) putDomainClaim(c *gin.Context) {
	var req struct {
		Domain    string `json:"domain" binding:"required"`
		Connector string `json:"connector"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if !emailDomainRe.MatchString(domain) {
		abort(c, http.StatusBadRequest, errors.New("that is not a domain name (acme.com)"))
		return
	}
	d, err := s.store.PutDomainClaim(c.Request.Context(), s.workspace(c), domain, strings.TrimSpace(req.Connector))
	if err != nil {
		storeErr(c, err, "domain claim")
		return
	}
	s.audit(c, "", "workspace.domain.claim", domain, "connector "+firstNonEmpty(d.Connector, "-"))
	c.JSON(http.StatusOK, claimView(*d))
}

// verifyDomainClaim is POST /api/workspace/domain-claims/:domain/verify:
// looks the TXT record up and records the result.
func (s *Server) verifyDomainClaim(c *gin.Context) {
	domain := strings.ToLower(c.Param("domain"))
	claims, err := s.store.ListDomainClaims(c.Request.Context(), s.workspace(c))
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	var claim *store.DomainClaim
	for i := range claims {
		if claims[i].Domain == domain {
			claim = &claims[i]
		}
	}
	if claim == nil {
		abort(c, http.StatusNotFound, errors.New("domain claim not found"))
		return
	}
	lookup := s.lookupTXT
	if lookup == nil {
		lookup = func(ctx context.Context, name string) ([]string, error) {
			return net.DefaultResolver.LookupTXT(ctx, name)
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	records, _ := lookup(ctx, "_shpyrd-verify."+domain)
	want := "shpyrd-verify=" + claim.Token
	for _, r := range records {
		if strings.TrimSpace(r) == want {
			d, err := s.store.MarkDomainVerified(c.Request.Context(), s.workspace(c), domain, time.Now())
			if err != nil {
				storeErr(c, err, "domain claim")
				return
			}
			s.audit(c, "", "workspace.domain.verified", domain, "")
			c.JSON(http.StatusOK, claimView(*d))
			return
		}
	}
	abort(c, http.StatusConflict, fmt.Errorf("no TXT record %q with value %q yet (DNS changes can take a few minutes)", "_shpyrd-verify."+domain, want))
}

func (s *Server) deleteDomainClaim(c *gin.Context) {
	domain := strings.ToLower(c.Param("domain"))
	if err := s.store.DeleteDomainClaim(c.Request.Context(), s.workspace(c), domain); err != nil {
		storeErr(c, err, "domain claim")
		return
	}
	s.audit(c, "", "workspace.domain.unclaim", domain, "")
	c.Status(http.StatusNoContent)
}

// ---- admission at sign-in ------------------------------------------------------

// admitSignIn decides whether a person may sign in (RFC-0033 phase 3):
// suspended people may not; accounts of a verified domain claim with a
// connector must arrive through that connector; someone signing in for the
// first time must satisfy the workspace's join policy. Operators (token,
// kubeconfig) are not people and always pass.
func (s *Server) admitSignIn(ctx context.Context, ws string, id ext.Identity) error {
	if id.Email == "" || id.Provider == "token" || id.Provider == "kubeconfig" {
		return nil
	}
	email := strings.ToLower(id.Email)
	if ws == "" {
		ws = store.DefaultWorkspace
	}
	people, err := s.store.ListIdentities(ctx, ws)
	if err != nil {
		return nil // the store is down: sign-in must not depend on it
	}
	var known *store.Identity
	for i := range people {
		if people[i].Email == email {
			known = &people[i]
		}
	}
	if known != nil && known.Status == store.StatusSuspended {
		return errors.New("your access is suspended; ask an administrator")
	}
	emailDomain := ""
	if i := strings.LastIndex(email, "@"); i > 0 {
		emailDomain = email[i+1:]
	}
	claims, _ := s.store.ListDomainClaims(ctx, ws)
	var claimed *store.DomainClaim
	for i := range claims {
		if claims[i].Domain == emailDomain && claims[i].VerifiedAt != nil {
			claimed = &claims[i]
		}
	}
	if claimed != nil && claimed.Connector != "" && id.Provider != claimed.Connector {
		label := claimed.Connector
		if s.rp != nil {
			if p := s.rp.provider(claimed.Connector); p != nil {
				label = p.Label
			}
		}
		return fmt.Errorf("accounts of %s sign in with %s", emailDomain, label)
	}
	if known != nil {
		return nil
	}
	w, err := s.store.Workspace(ctx, ws)
	if err != nil {
		return nil
	}
	switch w.Settings.JoinPolicy {
	case store.JoinCompany:
		if claimed == nil {
			return fmt.Errorf("only accounts of the company's domain can join; ask an administrator to add you")
		}
	case store.JoinListed:
		snap, err := s.authz.SnapshotFor(ctx, ws)
		if err != nil {
			return nil
		}
		for _, t := range snap.Teams {
			for _, m := range t.Members {
				if m == email {
					return nil
				}
			}
		}
		for _, g := range snap.Grants {
			if g.User == email {
				return nil
			}
		}
		return errors.New("only people already added to a team may join; ask an administrator to add you")
	}
	return nil
}

func (s *Server) listPeople(c *gin.Context) {
	ids, err := s.store.ListIdentities(c.Request.Context(), s.workspace(c))
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]PersonView, 0, len(ids))
	for _, id := range ids {
		out = append(out, personView(id))
	}
	c.JSON(http.StatusOK, out)
}

// setPersonStatus is PATCH /api/workspace/people/:email {status}: suspend
// a person (no role anywhere, no app opens, at once) or reactivate them.
func (s *Server) setPersonStatus(c *gin.Context) {
	var req struct {
		Status string `json:"status" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if req.Status != store.StatusActive && req.Status != store.StatusSuspended {
		abort(c, http.StatusBadRequest, errors.New("status must be active or suspended"))
		return
	}
	email := strings.ToLower(c.Param("email"))
	if me, ok := ext.IdentityFrom(c); ok && strings.EqualFold(me.Email, email) && req.Status == store.StatusSuspended {
		abort(c, http.StatusBadRequest, errors.New("you cannot suspend yourself"))
		return
	}
	id, err := s.store.SetIdentityStatus(c.Request.Context(), s.workspace(c), email, req.Status)
	if err != nil {
		storeErr(c, err, "person")
		return
	}
	s.membershipChanged()
	s.audit(c, "", "workspace.person."+req.Status, email, "")
	c.JSON(http.StatusOK, personView(*id))
}

// forgetPerson removes the record of a person; grants and team memberships
// are by email and stay (remove those explicitly). Sign-in recreates the
// record.
func (s *Server) forgetPerson(c *gin.Context) {
	email := strings.ToLower(c.Param("email"))
	if err := s.store.DeleteIdentity(c.Request.Context(), s.workspace(c), email); err != nil {
		storeErr(c, err, "person")
		return
	}
	s.audit(c, "", "workspace.person.forget", email, "")
	c.Status(http.StatusNoContent)
}

// recordSignIn notes an identity in the workspace's people. Tokens and the
// admin token are not people. Failures are logged, never surfaced: a sign-in
// must not depend on the store.
func (s *Server) recordSignIn(c *gin.Context, id ext.Identity) {
	if id.Email == "" || id.Provider == "token" || id.Provider == "kubeconfig" || id.Subject == "admin-token" {
		return
	}
	if _, err := s.store.TouchIdentity(c.Request.Context(), s.workspace(c), store.Identity{Email: id.Email, Name: id.Name, Provider: id.Provider, Groups: id.Groups}); err != nil {
		s.log.Warn("could not record sign-in", "email", id.Email, "err", err.Error())
	}
}

// exportWorkspace is GET /api/workspace/export: the store's content as the
// platform backup carries it (RFC-0037).
func (s *Server) exportWorkspace(c *gin.Context) {
	dump, err := s.store.Export(c.Request.Context(), s.workspace(c))
	if err != nil {
		storeErr(c, err, "workspace")
		return
	}
	c.JSON(http.StatusOK, dump)
}

// importWorkspace is POST /api/workspace/import[?overwrite=true]: teams,
// grants and people from a backup (`shpyrd cluster restore`).
func (s *Server) importWorkspace(c *gin.Context) {
	var dump store.Dump
	if err := c.ShouldBindJSON(&dump); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	if dump.Version > store.DumpVersion {
		abort(c, http.StatusBadRequest, errors.New("this backup was made by a newer shpyrd; upgrade first"))
		return
	}
	res, err := s.store.Import(c.Request.Context(), s.workspace(c), &dump, c.Query("overwrite") == "true")
	if err != nil {
		storeErr(c, err, "workspace")
		return
	}
	s.membershipChanged()
	s.audit(c, "", "workspace.import", "store", "teams "+itoa(res.Teams)+", grants "+itoa(res.Grants)+", people "+itoa(res.Identities))
	c.JSON(http.StatusOK, gin.H{"teams": res.Teams, "grants": res.Grants, "people": res.Identities, "skipped": res.Skipped})
}

func itoa(n int) string { return strconv.Itoa(n) }

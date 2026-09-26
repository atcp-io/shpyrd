package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/authz"
	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/store"
)

// Per-user API tokens (RFC-0031): shp_<id>_<random> bearer credentials
// with scoped roles, expiry and a last-used time. The random part is shown
// once and not stored; only its SHA-256 hash is kept.

const tokenPrefix = "shp_"

// GenerateToken mints a token value and returns (value, id, hash).
func GenerateToken() (value, id, hash string, err error) {
	rawID := make([]byte, 8)
	rawRand := make([]byte, 24)
	if _, err = rand.Read(rawID); err != nil {
		return
	}
	if _, err = rand.Read(rawRand); err != nil {
		return
	}
	id = hex.EncodeToString(rawID)
	randHex := hex.EncodeToString(rawRand)
	value = tokenPrefix + id + "_" + randHex
	sum := sha256.Sum256([]byte(randHex))
	hash = hex.EncodeToString(sum[:])
	return
}

// ParseToken extracts (id, randHex) from shp_<id>_<rand>.
func ParseToken(tok string) (id, randHex string, ok bool) {
	if !strings.HasPrefix(tok, tokenPrefix) {
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(tok, tokenPrefix), "_", 2)
	if len(parts) != 2 || len(parts[0]) != 16 || len(parts[1]) != 48 {
		return
	}
	return parts[0], parts[1], true
}

// TokenHash computes the stored hash from the random part.
func TokenHash(randHex string) string {
	sum := sha256.Sum256([]byte(randHex))
	return hex.EncodeToString(sum[:])
}

// ---- token middleware ---------------------------------------------------------

// identifyWithToken tries an shp_... token; returns true when it was accepted.
func (s *Server) identifyWithToken(c *gin.Context) bool {
	tok := c.GetHeader("X-Shpyrd-Token")
	if h := c.GetHeader("Authorization"); tok == "" && len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		tok = strings.TrimSpace(h[7:])
	}
	if !strings.HasPrefix(tok, tokenPrefix) {
		return false
	}
	_, randHex, ok := ParseToken(tok)
	if !ok {
		return false
	}
	hash := TokenHash(randHex)
	apiTok, err := s.store.LookupToken(c.Request.Context(), hash)
	if err != nil || apiTok == nil {
		return false
	}
	// Intersect with the owner's current roles so a demoted owner cannot
	// keep power through an old token, and a suspended owner's tokens stop
	// with them. The owner is resolved as the person the workspace knows
	// (email, provider and last groups claim); a token whose owner the
	// workspace has never seen carries nothing.
	snap, err := s.authz.SnapshotFor(c.Request.Context(), s.workspace(c))
	if err != nil {
		return false
	}
	ownerRoles, ok := s.tokenOwnerRoles(c, snap, apiTok)
	if !ok {
		return false
	}
	// Token roles are the intersection: token cannot exceed what the owner has.
	platform := minPlatformRole(apiTok.PlatformRole, ownerRoles.Platform)
	projects := map[string]string{}
	for proj, role := range apiTok.ProjectRoles {
		ownerRole := ownerRoles.Projects[proj]
		if authz.RankRole(role) <= authz.RankRole(ownerRole) {
			projects[proj] = role
		} else {
			projects[proj] = ownerRole
		}
	}
	id := ext.Identity{
		Subject:  "token:" + apiTok.ID,
		Email:    apiTok.OwnerEmail,
		Name:     apiTok.Name,
		Provider: "api-token",
	}
	// Build a synthetic authz.Roles from the token's grants.
	roles := authz.Roles{
		Platform: platform,
		Projects: projects,
		Enforced: ownerRoles.Enforced,
	}
	ext.SetIdentity(c, id)
	c.Set("shpyrd.roles", roles)
	return true
}

// tokenOwnerRoles resolves the roles the token's owner holds right now.
// Admin-token holders (owner email empty) are platform admins by
// definition; everyone else must be an active person of the workspace.
func (s *Server) tokenOwnerRoles(c *gin.Context, snap *authz.Snapshot, t *store.APIToken) (authz.Roles, bool) {
	ctx := c.Request.Context()
	// A token only works at the workspace it was created in.
	ws, err := s.store.Workspace(ctx, s.workspace(c))
	if err != nil || ws.ID != t.WorkspaceID {
		return authz.Roles{}, false
	}
	if t.OwnerEmail == "" {
		return snap.RolesFor(ext.Identity{Subject: "admin-token", Provider: "token"}), true
	}
	person, err := s.store.GetIdentity(ctx, ws.Slug, t.OwnerEmail)
	if err != nil || person == nil {
		return authz.Roles{}, false
	}
	roles := snap.RolesFor(ext.Identity{
		Subject:  person.ID,
		Email:    person.Email,
		Name:     person.Name,
		Provider: person.Provider,
		Groups:   person.Groups,
	})
	if roles.Suspended {
		return authz.Roles{}, false
	}
	return roles, true
}

func minPlatformRole(a, b string) string {
	if authz.PlatformRank(a) < authz.PlatformRank(b) {
		return a
	}
	return b
}

// ---- API handlers ------------------------------------------------------------

// TokenView is a token as returned by the API (no secret).
type TokenView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	OwnerEmail   string            `json:"ownerEmail,omitempty"`
	PlatformRole string            `json:"platformRole,omitempty"`
	ProjectRoles map[string]string `json:"projectRoles,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
	ExpiresAt    *time.Time        `json:"expiresAt,omitempty"`
	LastUsedAt   *time.Time        `json:"lastUsedAt,omitempty"`
}

// TokenCreateView is the create response: the value is shown once.
type TokenCreateView struct {
	TokenView
	Token string `json:"token"`
}

func tokenView(t store.APIToken) TokenView {
	return TokenView{ID: t.ID, Name: t.Name, OwnerEmail: t.OwnerEmail, PlatformRole: t.PlatformRole, ProjectRoles: t.ProjectRoles, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt}
}

// listTokens is GET /api/tokens: the caller's own tokens (platform admin
// sees all).
func (s *Server) listTokens(c *gin.Context) {
	roles, err := s.rolesOf(c)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	id, _ := ext.IdentityFrom(c)
	email := ""
	if !roles.Can(authz.ClusterAdmin, "") {
		email = id.Email
	}
	tokens, err := s.store.ListTokens(c.Request.Context(), s.workspace(c), email)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]TokenView, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, tokenView(t))
	}
	c.JSON(http.StatusOK, out)
}

// createToken is POST /api/tokens.
type tokenCreateRequest struct {
	Name         string            `json:"name" binding:"required"`
	PlatformRole string            `json:"platformRole,omitempty"`
	ProjectRoles map[string]string `json:"projectRoles,omitempty"`
	ExpiresIn    string            `json:"expiresIn,omitempty"` // "30d", "90d", "365d"
}

func (s *Server) createToken(c *gin.Context) {
	var req tokenCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 {
		abort(c, http.StatusBadRequest, errors.New("name must be 1 to 80 characters"))
		return
	}
	// A token never mints tokens: a leaked CI credential must not be able
	// to grant itself persistence. People (sessions) and the admin token do.
	if id, ok := ext.IdentityFrom(c); ok && id.Provider == "api-token" {
		abort(c, http.StatusForbidden, fmt.Errorf("an API token cannot create tokens: sign in to the dashboard (Workspace, API tokens) or use `shpyrd login` with the admin token"))
		return
	}
	// Validate roles are within the caller's own.
	caller, _err2 := s.rolesOf(c)
	if _err2 != nil {
		abort(c, http.StatusBadGateway, _err2)
		return
	}
	callerID, _ := ext.IdentityFrom(c)
	if req.PlatformRole != "" {
		if authz.PlatformRank(req.PlatformRole) > authz.PlatformRank(caller.Platform) {
			abort(c, http.StatusBadRequest, fmt.Errorf("a token cannot have platform role %q: you are %s", req.PlatformRole, roleOrNone(caller.Platform)))
			return
		}
	}
	for proj, role := range req.ProjectRoles {
		ownerRole := caller.Projects[proj]
		if authz.RankRole(role) > authz.RankRole(ownerRole) {
			abort(c, http.StatusBadRequest, fmt.Errorf("a token cannot be %s on %s: you are %s there", role, proj, roleOrNone(ownerRole)))
			return
		}
		switch role {
		case v1alpha1.RoleUser, v1alpha1.RoleViewer, v1alpha1.RoleDeveloper, v1alpha1.RoleAdmin:
		default:
			abort(c, http.StatusBadRequest, fmt.Errorf("invalid role %q", role))
			return
		}
	}
	var exp *time.Time
	if req.ExpiresIn != "" {
		d, err := parseDuration(req.ExpiresIn)
		if err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
		t := time.Now().Add(d)
		exp = &t
	}
	value, _, hash, err := GenerateToken()
	if err != nil {
		abort(c, http.StatusInternalServerError, err)
		return
	}
	tok := store.APIToken{
		Name:         req.Name,
		OwnerEmail:   callerID.Email,
		PlatformRole: req.PlatformRole,
		ProjectRoles: req.ProjectRoles,
		ExpiresAt:    exp,
	}
	created, err := s.store.CreateToken(c.Request.Context(), s.workspace(c), tok, hash)
	if err != nil {
		storeErr(c, err, "token")
		return
	}
	s.audit(c, "", "token.create", req.Name, "")
	c.JSON(http.StatusCreated, TokenCreateView{TokenView: tokenView(*created), Token: value})
}

// roleOrNone names a role in error messages, "no one" when empty.
func roleOrNone(role string) string {
	if role == "" {
		return "not granted any role"
	}
	return role
}

// deleteToken is DELETE /api/tokens/:id.
func (s *Server) deleteToken(c *gin.Context) {
	id := c.Param("id")
	caller, _err3 := s.rolesOf(c)
	if _err3 != nil {
		return
	}
	callerID, _ := ext.IdentityFrom(c)
	// Owners may revoke their own; admins may revoke any.
	tokens, err := s.store.ListTokens(c.Request.Context(), s.workspace(c), "")
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	var target *store.APIToken
	for i := range tokens {
		if tokens[i].ID == id {
			target = &tokens[i]
		}
	}
	if target == nil {
		abort(c, http.StatusNotFound, errors.New("token not found"))
		return
	}
	if !strings.EqualFold(target.OwnerEmail, callerID.Email) && !caller.Can(authz.ClusterAdmin, "") {
		abort(c, http.StatusForbidden, errors.New("you may only revoke your own tokens"))
		return
	}
	if err := s.store.DeleteToken(c.Request.Context(), s.workspace(c), id); err != nil {
		storeErr(c, err, "token")
		return
	}
	s.audit(c, "", "token.revoke", target.Name, "")
	c.Status(http.StatusNoContent)
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if strings.HasSuffix(s, "d") {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err != nil || days <= 0 || days > 3650 {
			return 0, fmt.Errorf("expiresIn must be like 30d, 90d, 365d (max 3650d)")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("expiresIn must be like 30d, 90d, 365d")
}

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"shpyrd/pkg/edge"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/store"
)

// Realms decides which login methods a workspace offers (RFC-0033's
// RealmProvider). The open-source platform offers every configured method
// to its one workspace; the cloud layer narrows them per workspace and
// adds its console pool.
type Realms interface {
	// Methods filters the platform's login methods for a workspace. The
	// password form (auth-local) is listed among them by id when enabled.
	Methods(ctx context.Context, ws *store.Workspace, all AuthConfig) AuthConfig
}

// allMethods is the open-source Realms: everything, everywhere.
type allMethods struct{}

func (allMethods) Methods(_ context.Context, _ *store.Workspace, all AuthConfig) AuthConfig {
	return all
}

// authConfigFor is the sign-in configuration the request's workspace shows.
func (s *Server) authConfigFor(c *gin.Context) AuthConfig {
	all := s.authConfig()
	ws, err := s.tenant(c)
	if err != nil {
		return all
	}
	return s.realms.Methods(c.Request.Context(), ws, all)
}

// offers reports whether a workspace lists a login method ("" means the
// only one, when there is exactly one).
func (s *Server) offers(ctx context.Context, ws *store.Workspace, providerID string) bool {
	cfg := s.realms.Methods(ctx, ws, s.authConfig())
	if providerID == "" {
		return len(cfg.Providers) == 1 || cfg.Password != nil
	}
	if cfg.Password != nil && cfg.Password.ID == providerID {
		return true
	}
	for _, p := range cfg.Providers {
		if p.ID == providerID {
			return true
		}
	}
	return false
}

// ---- console-hosted sign-in (RFC-0033 phase 6, option 1) ------------------------
//
// The bundled issuer has one relying party: the console, which is the
// implicit workspace's dashboard. A workspace at its own host cannot
// complete an OpenID Connect flow itself, so its /api/auth/login sends the
// browser to the console's, naming the workspace; the console signs the
// person in, applies the *workspace's* admission (join policy, claimed
// domains, suspension), and hands a one-time code back to the workspace
// host, which mints the workspace's session. No console session is
// opened for the person: the console only vouches.

// consoleWorkspace is the slug of the workspace whose dashboard is the
// relying party.
const consoleWorkspace = store.DefaultWorkspace

// atConsole says the request arrived at the console (the implicit
// workspace's host) rather than at an explicit workspace's.
func (s *Server) atConsole(c *gin.Context) bool {
	ws, err := s.tenant(c)
	return err == nil && ws.Slug == consoleWorkspace
}

// requireConsole keeps a route for the console: at an explicit workspace's
// host it is not there.
func (s *Server) requireConsole() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.atConsole(c) {
			abort(c, http.StatusNotFound, errors.New("not available in this workspace: the platform operator manages this"))
			return
		}
		c.Next()
	}
}

// sessionHandoff is what the console hands a workspace host: who signed
// in and how.
type sessionHandoff struct {
	Identity ext.Identity `json:"identity"`
	IDToken  string       `json:"idToken,omitempty"`
	How      string       `json:"how"`
}

// consoleLoginURL is the console's login URL for a workspace: the browser
// comes back to next on the workspace host afterwards.
func (s *Server) consoleLoginURL(provider string, ws *store.Workspace, next string) string {
	q := url.Values{"workspace": {ws.Slug}, "next": {safeNext(next)}}
	if provider != "" {
		q.Set("provider", provider)
	}
	return s.opts.Public.DashboardURL + "/api/auth/login?" + q.Encode()
}

// handoffTarget resolves the workspace a console login is on behalf of.
func (s *Server) handoffTarget(ctx context.Context, slug string) (*store.Workspace, error) {
	ws, err := s.store.Workspace(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("unknown workspace %q", slug)
	}
	if ws.Address == "" {
		return nil, fmt.Errorf("workspace %q has no address of its own", slug)
	}
	if ws.Status == store.WorkspaceSuspended {
		return nil, errors.New("this workspace is suspended")
	}
	return ws, nil
}

// handoff completes a console login made on behalf of a workspace: the
// workspace's admission runs, then a one-time code travels to its host.
func (s *Server) handoff(c *gin.Context, target *store.Workspace, id ext.Identity, idToken, next string) {
	if err := s.admitSignIn(c.Request.Context(), target.Slug, id); err != nil {
		s.auditFailure(c, "auth.refused", id.Email, err.Error()+" (workspace "+target.Slug+")")
		s.loginFailedAt(c, target, err)
		return
	}
	host := s.dashboardHostOf(target)
	code, err := s.edgeCodes.MintJSON(c.Request.Context(), host, edge.KindSession, sessionHandoff{Identity: id, IDToken: idToken, How: "console"})
	if err != nil {
		abort(c, http.StatusInternalServerError, err)
		return
	}
	c.Redirect(http.StatusFound, "https://"+host+edgePathPrefix+"session?"+url.Values{"code": {code}, "rd": {safeNext(next)}}.Encode())
}

// edgeSession is GET /.shpyrd/session?code=&rd= at a workspace host: the
// console's code becomes this workspace's session.
func (s *Server) edgeSession(c *gin.Context) {
	if s.atConsole(c) {
		s.edgePage(c, http.StatusNotFound, "Nothing here", "Sign in from the dashboard.", nil)
		return
	}
	var h sessionHandoff
	if err := s.edgeCodes.RedeemJSON(c.Request.Context(), c.Query("code"), c.Request.Host, edge.KindSession, &h); err != nil {
		s.edgePage(c, http.StatusBadRequest, "Sign-in link expired", "Go back to the dashboard and sign in again.", map[string]string{"Dashboard": "/"})
		return
	}
	if h.Identity.Email == "" && h.Identity.Subject == "" {
		s.edgePage(c, http.StatusBadRequest, "Sign-in failed", "The sign-in carried no identity.", nil)
		return
	}
	if _, ok := s.openSession(c, h.Identity, h.IDToken, firstNonEmpty(h.How, "console")); !ok {
		return
	}
	c.Redirect(http.StatusFound, safeNext(c.Query("rd")))
}

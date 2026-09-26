package api

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/authz"
	"github.com/shpyrd-io/shpyrd/pkg/edge"
	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/project"
	"github.com/shpyrd-io/shpyrd/pkg/store"
)

// The edge (RFC-0033): ingress-nginx asks /edge/auth about every request to
// an app whose access is not public. The caller is known by a cookie set on
// the app's own host — never the dashboard's session cookie, which the app
// would otherwise receive — or by a platform token. The answer is yes with
// the identity in headers and a signed JWT, "sign in" (401, nginx redirects
// to /.shpyrd/signin), or no (403, nginx shows /.shpyrd's denied page).
//
// Signing in on an app host: /.shpyrd/signin sends the browser to the
// dashboard host's /.shpyrd/start, which has the session (or asks for one),
// mints a one-time code and sends the browser back to the app host's
// /.shpyrd/callback, which sets the app-host cookie bound to that project.

const (
	edgeCookieSecure   = "__Host-shpyrd_edge"
	edgeCookieInsecure = "shpyrd_edge"
	edgePathPrefix     = "/.shpyrd/"
)

// edgeCookieName depends on HTTPS: the __Host- prefix needs Secure.
func (s *Server) edgeCookieName() string {
	if s.secureCookies() {
		return edgeCookieSecure
	}
	return edgeCookieInsecure
}

// hostOnly strips a port.
func hostOnly(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// dashboardHost is the host (with port) of the platform's dashboard URL:
// where the implicit workspace answers.
func (s *Server) dashboardHost() string {
	if u, err := url.Parse(s.opts.Public.DashboardURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "shpyrd." + s.opts.Public.Domain
}

// withPort appends the platform's HTTPS port when it is not the default.
func (s *Server) withPort(host string) string {
	if p := s.opts.Public.HTTPSPort; p != "" && p != "443" {
		return host + ":" + p
	}
	return host
}

// Names of a workspace (RFC-0033 phase 6). The implicit workspace answers
// at the platform's names (shpyrd.<domain>, <app>.<domain>); an explicit
// one at its address (<address>, <app>.<address>).

// dashboardHostOf is the host (with port) of a workspace's dashboard.
func (s *Server) dashboardHostOf(ws *store.Workspace) string {
	if ws != nil && ws.Address != "" {
		return s.withPort(ws.Address)
	}
	return s.dashboardHost()
}

// dashboardURLOf is the URL of a workspace's dashboard.
func (s *Server) dashboardURLOf(ws *store.Workspace) string {
	if ws != nil && ws.Address != "" {
		return "https://" + s.withPort(ws.Address)
	}
	return s.opts.Public.DashboardURL
}

// appsDomainOf is the domain a workspace's apps live one label under.
func (s *Server) appsDomainOf(ws *store.Workspace) string {
	if ws != nil && ws.Address != "" {
		return ws.Address
	}
	return s.opts.Public.Domain
}

// appPublicHostIn is <slug>.<apps domain>[:port]: an app's default address
// in its workspace.
func (s *Server) appPublicHostIn(ws *store.Workspace, slug string) string {
	return s.withPort(slug + "." + s.appsDomainOf(ws))
}

// dashboardURLFor is the dashboard URL of the request's workspace; the
// platform's when the host resolves to none (error pages for unknown hosts).
func (s *Server) dashboardURLFor(c *gin.Context) string {
	ws, err := s.tenant(c)
	if err != nil {
		return s.opts.Public.DashboardURL
	}
	return s.dashboardURLOf(ws)
}

// appByHost finds the app an incoming host belongs to: its default address
// or one of its custom domains. Cached briefly: nginx asks on every request.
func (s *Server) appByHost(c *gin.Context, host string) (*shpyrdv1.App, error) {
	host = hostOnly(host)
	s.hostCache.mu.Lock()
	if s.hostCache.apps != nil && time.Since(s.hostCache.at) < 15*time.Second {
		app := s.hostCache.apps[host]
		s.hostCache.mu.Unlock()
		if app == nil {
			return nil, errors.New("no app at this address")
		}
		return app, nil
	}
	s.hostCache.mu.Unlock()
	var list shpyrdv1.AppList
	if err := s.apps.List(c.Request.Context(), &list); err != nil {
		return nil, err
	}
	// Each app answers one label under its workspace's apps domain.
	domains := map[string]string{store.DefaultWorkspace: s.opts.Public.Domain}
	if all, err := s.store.ListWorkspaces(c.Request.Context()); err == nil {
		for i := range all {
			domains[all[i].Slug] = s.appsDomainOf(&all[i])
		}
	}
	index := map[string]*shpyrdv1.App{}
	for i := range list.Items {
		app := &list.Items[i]
		domain, ok := domains[workspaceOf(app)]
		if !ok {
			continue // an app of a workspace this server does not know
		}
		index[strings.ToLower(app.Name+"."+domain)] = app
		for _, d := range app.Spec.Domains {
			index[hostOnly(d)] = app
		}
	}
	s.hostCache.mu.Lock()
	s.hostCache.apps, s.hostCache.at = index, time.Now()
	s.hostCache.mu.Unlock()
	if app := index[host]; app != nil {
		return app, nil
	}
	return nil, errors.New("no app at this address")
}

type hostCache struct {
	mu   sync.Mutex
	apps map[string]*shpyrdv1.App
	at   time.Time
}

// edgeCaller is who /edge/auth found.
type edgeCaller struct {
	identity ext.Identity
	session  string
	preview  *edge.Preview
}

// edgeIdentify resolves the caller of an app request: a platform token, or
// the app-host cookie whose dashboard session must still exist.
func (s *Server) edgeIdentify(c *gin.Context, project string) (*edgeCaller, error) {
	tok := c.GetHeader("X-Shpyrd-Token")
	if tok == "" {
		if h := c.GetHeader("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			tok = strings.TrimSpace(h[7:])
		}
	}
	if tok != "" {
		if s.opts.Token != "" && !s.opts.TokenDisabled && subtle.ConstantTimeCompare([]byte(tok), []byte(s.opts.Token)) == 1 {
			return &edgeCaller{identity: ext.Identity{Subject: "admin-token", Provider: "token", Admin: true}}, nil
		}
		return nil, errors.New("invalid token")
	}
	raw, err := c.Cookie(s.edgeCookieName())
	if err != nil || raw == "" {
		return nil, nil // anonymous
	}
	claims, err := s.edgeKeys.VerifyCookie(raw, project)
	if err != nil {
		return nil, nil // a stale or foreign cookie is just anonymous
	}
	if s.rp == nil {
		return nil, nil
	}
	sess, ok := s.rp.sessions.getIn(claims.SessionID, s.workspaceID(c))
	if !ok {
		return nil, nil // signed out, or a session of another workspace
	}
	return &edgeCaller{identity: sess.Identity, session: claims.SessionID, preview: claims.Preview}, nil
}

// edgeWorkspace finds the workspace an auth subrequest is about. nginx
// calls /edge/auth at the service name, so the host says nothing; the
// Ingress annotation may name the workspace, and X-Original-URL carries
// the app host the visitor used. Neither: the implicit workspace.
func (s *Server) edgeWorkspace(c *gin.Context) (*store.Workspace, error) {
	if v, ok := c.Get(ctxWorkspace); ok {
		return v.(*store.Workspace), nil
	}
	var ws *store.Workspace
	var err error
	if slug := c.Query("workspace"); slug != "" {
		ws, err = s.store.Workspace(c.Request.Context(), slug)
	} else if u, perr := url.Parse(c.GetHeader("X-Original-URL")); perr == nil && u.Host != "" {
		ws, err = s.tenancy.Resolve(c.Request.Context(), u.Host)
	} else {
		ws, err = s.tenancy.Resolve(c.Request.Context(), c.Request.Host)
	}
	if err != nil {
		return nil, err
	}
	c.Set(ctxWorkspace, ws)
	return ws, nil
}

// edgeAuth is GET /edge/auth?project=<slug>&mode=<authenticated|identified>
// [&workspace=<slug>].
func (s *Server) edgeAuth(c *gin.Context) {
	slug := c.Query("project")
	if !project.ValidSlug(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project is required"})
		return
	}
	ws, err := s.edgeWorkspace(c)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "no workspace answers at this address"})
		return
	}
	if ws.Status == store.WorkspaceSuspended {
		c.JSON(http.StatusForbidden, gin.H{"error": "this workspace is suspended"})
		return
	}
	mode := c.DefaultQuery("mode", shpyrdv1.AccessAuthenticated)
	caller, err := s.edgeIdentify(c, slug)
	if err != nil {
		c.Header("WWW-Authenticate", `Bearer realm="shpyrd"`)
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	// Nothing from the client survives; every header below is set by us.
	for _, h := range []string{"X-Shpyrd-User", "X-Shpyrd-Email", "X-Shpyrd-Name", "X-Shpyrd-Teams", "X-Shpyrd-Roles", "Authorization"} {
		c.Header(h, "")
	}
	if caller == nil || (caller.preview != nil && caller.preview.Anonymous) {
		if mode == shpyrdv1.AccessIdentified {
			c.Status(http.StatusOK)
			return
		}
		if caller != nil { // an anonymous preview of a closed app
			c.JSON(http.StatusForbidden, gin.H{"error": "anonymous visitors are asked to sign in", "preview": true})
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "sign in to open this app"})
		return
	}
	snap, err := s.authz.SnapshotFor(c.Request.Context(), s.workspace(c))
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	roles := snap.RolesFor(caller.identity)
	if roles.Suspended {
		c.JSON(http.StatusForbidden, gin.H{"error": "your access is suspended"})
		return
	}
	teams := snap.TeamNames(caller.identity)
	projectRole := roles.ProjectRole(slug)
	claims := edge.Claims{
		Issuer: s.dashboardURLOf(ws), Subject: caller.identity.Subject, Audience: slug,
		Email: caller.identity.Email, Name: caller.identity.Name, Workspace: ws.Slug, Project: slug,
		Realm: "workspace", Provider: caller.identity.Provider,
	}
	if caller.identity.Provider == "token" {
		claims.Realm = "operator"
	}
	if caller.preview != nil {
		// Open as: the app sees the chosen teams and what they may do.
		teams = caller.preview.Teams
		projectRole = snap.RoleForTeams(slug, teams)
		claims.Preview = true
		claims.Actor = &edge.Actor{Subject: caller.identity.Subject, Email: caller.identity.Email}
		if projectRole == "" && mode != shpyrdv1.AccessIdentified {
			c.JSON(http.StatusForbidden, gin.H{"error": "these teams may not open this app", "preview": true})
			return
		}
	} else if !roles.Can(authz.ProjectOpen, slug) && mode != shpyrdv1.AccessIdentified {
		c.JSON(http.StatusForbidden, gin.H{"error": "you may not open this app"})
		return
	}
	var roleList []string
	if projectRole != "" {
		roleList = append(roleList, projectRole)
	}
	if caller.preview == nil && roles.Platform != "" {
		roleList = append(roleList, roles.Platform)
	}
	if roleList == nil {
		roleList = []string{}
	}
	if teams == nil {
		teams = []string{}
	}
	claims.Roles, claims.Teams = roleList, teams
	now := time.Now()
	claims.IssuedAt, claims.ExpiresAt = now.Unix(), now.Add(edge.TokenTTL).Unix()
	jwt, err := s.edgeKeys.Sign("JWT", claims)
	if err != nil {
		abort(c, http.StatusInternalServerError, err)
		return
	}
	user := firstNonEmpty(caller.identity.Email, caller.identity.Subject)
	c.Header("X-Shpyrd-User", user)
	c.Header("X-Shpyrd-Email", caller.identity.Email)
	c.Header("X-Shpyrd-Name", caller.identity.Name)
	c.Header("X-Shpyrd-Teams", strings.Join(teams, ","))
	c.Header("X-Shpyrd-Roles", strings.Join(roleList, ","))
	c.Header("Authorization", "Bearer "+jwt)
	c.Status(http.StatusOK)
}

// jwks publishes the verification key (GET /.well-known/jwks.json).
func (s *Server) jwks(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=300")
	c.JSON(http.StatusOK, s.edgeKeys.JWKS())
}

// edgeSignin is GET /.shpyrd/signin?rd=<uri> on an app host: off to the
// dashboard host, which knows the session.
func (s *Server) edgeSignin(c *gin.Context) {
	app, err := s.appByHost(c, c.Request.Host)
	if err != nil || workspaceOf(app) != s.workspace(c) {
		s.edgePage(c, http.StatusNotFound, "No app here", "There is no app at this address.", nil)
		return
	}
	// The app's workspace's dashboard signs its visitors in.
	ws, _ := s.tenant(c)
	q := url.Values{"app": {c.Request.Host}, "rd": {safeNext(c.Query("rd"))}}
	c.Redirect(http.StatusFound, "https://"+s.dashboardHostOf(ws)+edgePathPrefix+"start?"+q.Encode())
}

// edgeStart is GET /.shpyrd/start?app=<host>&rd=<uri> on the dashboard host.
func (s *Server) edgeStart(c *gin.Context) {
	appHost := strings.ToLower(c.Query("app"))
	app, err := s.appByHost(c, appHost)
	if err != nil || workspaceOf(app) != s.workspace(c) {
		// Unknown, or an app of another workspace: its own dashboard signs
		// people in, not this one.
		s.edgePage(c, http.StatusBadRequest, "Unknown app", "That address does not belong to an app of this workspace.", nil)
		return
	}
	rd := safeNext(c.Query("rd"))
	ok, _ := s.sessionAuth(c)
	if !ok {
		// The dashboard's login page brings the browser back here.
		self := edgePathPrefix + "start?" + url.Values{"app": {appHost}, "rd": {rd}}.Encode()
		c.Redirect(http.StatusFound, "/?next="+url.QueryEscape(self))
		return
	}
	sid, _ := c.Cookie(sessionCookie)
	code, err := s.edgeCodes.Mint(c.Request.Context(), appHost, edge.CookieClaims{SessionID: sid, Project: app.Name})
	if err != nil {
		abort(c, http.StatusInternalServerError, err)
		return
	}
	c.Redirect(http.StatusFound, "https://"+appHost+edgePathPrefix+"callback?"+url.Values{"code": {code}, "rd": {rd}}.Encode())
}

// edgeCallback is GET /.shpyrd/callback?code=&rd= on an app host: the code
// becomes the app-host cookie.
func (s *Server) edgeCallback(c *gin.Context) {
	app, err := s.appByHost(c, c.Request.Host)
	if err != nil {
		s.edgePage(c, http.StatusNotFound, "No app here", "There is no app at this address.", nil)
		return
	}
	claims, err := s.edgeCodes.Redeem(c.Request.Context(), c.Query("code"), c.Request.Host)
	if err != nil || claims.Project != app.Name || workspaceOf(app) != s.workspace(c) {
		s.edgePage(c, http.StatusBadRequest, "Sign-in link expired", "Open the app again to sign in.", nil)
		return
	}
	value, err := s.edgeKeys.SignCookie(*claims)
	if err != nil {
		abort(c, http.StatusInternalServerError, err)
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name: s.edgeCookieName(), Value: value, Path: "/", HttpOnly: true, Secure: s.secureCookies(),
		SameSite: http.SameSiteLaxMode, MaxAge: int(edge.CookieTTL.Seconds()),
	})
	c.Redirect(http.StatusFound, safeNext(c.Query("rd")))
}

// edgeLogout is GET /.shpyrd/logout on an app host: forget this app's
// cookie and go to the dashboard, where signing out ends the session.
func (s *Server) edgeLogout(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: s.edgeCookieName(), Value: "", Path: "/", HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	c.Redirect(http.StatusFound, s.dashboardURLFor(c))
}

// edgeDenied renders the page nginx shows for a 403 from /edge/auth: the
// custom-http-errors backend forwards the original request with X-Code.
func (s *Server) edgeDenied(c *gin.Context) {
	app, err := s.appByHost(c, c.Request.Host)
	name := "This app"
	if err == nil {
		name = project.DisplayName(app)
	}
	var teams []string
	if app != nil {
		if grants, err := s.store.ListProjectGrants(c.Request.Context(), s.workspace(c), app.Name); err == nil {
			seen := map[string]bool{}
			for _, g := range grants {
				if g.Team != "" && !seen[g.Team] {
					seen[g.Team] = true
					teams = append(teams, g.Team)
				}
			}
			sort.Strings(teams)
		}
	}
	// A suspended person gets the reason, not the team list.
	if app != nil {
		if caller, err := s.edgeIdentify(c, app.Name); err == nil && caller != nil {
			if roles, err := s.authz.RolesIn(c.Request.Context(), s.workspace(c), caller.identity); err == nil && roles.Suspended {
				s.edgePage(c, http.StatusForbidden, "Your access is suspended", "An administrator switched your access off. Ask them to reactivate it.", map[string]string{"Sign in as someone else": edgePathPrefix + "logout"})
				return
			}
		}
	}
	msg := name + " is available to its team"
	if len(teams) > 0 {
		msg = fmt.Sprintf("%s is available to the %s team", name, joinAnd(teams))
		if len(teams) > 1 {
			msg = fmt.Sprintf("%s is available to the %s teams", name, joinAnd(teams))
		}
	}
	if wantsJSON(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": msg, "teams": teams})
		return
	}
	links := map[string]string{"Sign in as someone else": edgePathPrefix + "logout", "Your apps": s.dashboardURLFor(c)}
	s.edgePage(c, http.StatusForbidden, msg, "Ask a project admin to grant your team access, or sign in with an account that has it.", links)
}

// foreignHost says whether a request arrived for a host that is neither
// its workspace's dashboard nor a local one (port-forwards, tests, health
// checks): an app host, or a host nobody claims.
func (s *Server) foreignHost(c *gin.Context) bool {
	h := hostOnly(c.Request.Host)
	if h == "" || h == "localhost" || net.ParseIP(h) != nil || !strings.Contains(h, ".") {
		return false
	}
	ws, err := s.tenant(c)
	if err != nil {
		return true
	}
	return h != hostOnly(s.dashboardHostOf(ws))
}

func wantsJSON(c *gin.Context) bool {
	accept := c.GetHeader("Accept")
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// edgePage is the small branded page the edge serves on app hosts.
func (s *Server) edgePage(c *gin.Context, status int, title, text string, links map[string]string) {
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>`)
	b.WriteString(html.EscapeString(title))
	b.WriteString(`</title><style>body{margin:0;font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0f172a;color:#e2e8f0;display:flex;min-height:100vh;align-items:center;justify-content:center}main{max-width:32rem;padding:2.5rem;background:#1e293b;border-radius:12px;border-top:4px solid #ff4f00}h1{font-size:1.25rem;margin:0 0 .75rem}p{margin:0 0 1.25rem;color:#cbd5e1}a{color:#ff7a3d;text-decoration:none;margin-right:1.25rem}a:hover{text-decoration:underline}small{color:#64748b}</style></head><body><main><h1>`)
	b.WriteString(html.EscapeString(title))
	b.WriteString(`</h1><p>`)
	b.WriteString(html.EscapeString(text))
	b.WriteString(`</p>`)
	keys := make([]string, 0, len(links))
	for k := range links {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(`<a href="` + html.EscapeString(links[k]) + `">` + html.EscapeString(k) + `</a>`)
	}
	b.WriteString(`<p><small>shpyrd</small></p></main></body></html>`)
	c.Header("Cache-Control", "no-store")
	c.Data(status, "text/html; charset=utf-8", []byte(b.String()))
}

// customError handles what ingress-nginx sends to its default backend:
// custom-http-errors of an app's Ingress (the edge's 403 becomes our page),
// and requests for hosts no Ingress serves (a "no app here" page instead of
// the dashboard, which lives on its own host only).
func (s *Server) customError(c *gin.Context) bool {
	code := c.GetHeader("X-Code")
	if code == "" {
		if s.foreignHost(c) {
			s.edgePage(c, http.StatusNotFound, "No app here", "There is no app at this address.", map[string]string{"Dashboard": s.dashboardURLFor(c)})
			return true
		}
		return false
	}
	switch code {
	case "403":
		s.edgeDenied(c)
	case "401":
		c.Redirect(http.StatusFound, edgePathPrefix+"signin?rd="+url.QueryEscape(c.GetHeader("X-Original-URI")))
	default:
		s.edgePage(c, http.StatusBadGateway, "The app is not answering", "Try again in a moment. Its logs and status are in the dashboard.", map[string]string{"Dashboard": s.dashboardURLFor(c)})
	}
	return true
}

// ---- preview and launcher ---------------------------------------------------

// previewRequest is POST /api/projects/:slug/preview: "Open as".
type previewRequest struct {
	Teams     []string `json:"teams"`
	Anonymous bool     `json:"anonymous"`
}

func (s *Server) previewApp(c *gin.Context) {
	var req previewRequest
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			abort(c, http.StatusBadRequest, err)
			return
		}
	}
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	if app.EffectiveAccess() == shpyrdv1.AccessPublic {
		abort(c, http.StatusBadRequest, errors.New("a public app looks the same to everyone; nothing to preview"))
		return
	}
	sid, _ := c.Cookie(sessionCookie)
	if sid == "" {
		abort(c, http.StatusBadRequest, errors.New("previews need a browser session: open the dashboard signed in"))
		return
	}
	teams := compact(req.Teams)
	for _, t := range teams {
		if _, err := s.store.GetTeam(c.Request.Context(), s.workspace(c), t); err != nil {
			abort(c, http.StatusNotFound, fmt.Errorf("team %q not found", t))
			return
		}
	}
	ws, _ := s.tenant(c)
	host := s.appPublicHostIn(ws, app.Name)
	code, err := s.edgeCodes.Mint(c.Request.Context(), host, edge.CookieClaims{SessionID: sid, Project: app.Name, Preview: &edge.Preview{Teams: teams, Anonymous: req.Anonymous}})
	if err != nil {
		abort(c, http.StatusInternalServerError, err)
		return
	}
	what := "anonymous"
	if !req.Anonymous {
		what = "teams " + strings.Join(teams, ", ")
		if len(teams) == 0 {
			what = "a member of no team"
		}
	}
	s.audit(c, app.Name, "project.preview", app.Name, "opened as "+what)
	c.JSON(http.StatusOK, gin.H{"url": "https://" + host + edgePathPrefix + "callback?" + url.Values{"code": {code}, "rd": {"/"}}.Encode()})
}

// LauncherApp is one tile of the launcher: an app the caller may open.
type LauncherApp struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"displayName"`
	URL         string `json:"url,omitempty"`
	Access      string `json:"access"`
	Phase       string `json:"phase"`
	Role        string `json:"role,omitempty"`
}

// launcher is GET /api/launcher: the apps the caller may open — by role,
// or because they are public.
func (s *Server) launcher(c *gin.Context) {
	var list shpyrdv1.AppList
	if err := s.apps.List(c.Request.Context(), &list); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	roles, err := s.rolesOf(c)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := []LauncherApp{}
	ws := s.workspace(c)
	for i := range list.Items {
		a := &list.Items[i]
		if workspaceOf(a) != ws {
			continue
		}
		access := a.EffectiveAccess()
		if access == shpyrdv1.AccessAuthenticated && !roles.Can(authz.ProjectOpen, a.Name) {
			continue
		}
		out = append(out, LauncherApp{Slug: a.Name, DisplayName: project.DisplayName(a), URL: a.Status.URL, Access: access, Phase: firstNonEmpty(a.Status.Phase, shpyrdv1.PhasePending), Role: roles.ProjectRole(a.Name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DisplayName < out[j].DisplayName })
	c.JSON(http.StatusOK, out)
}

// workspaceOf is the workspace an App belongs to, from its authoritative
// label; Apps from before RFC-0033 carry none and are the implicit one.
func workspaceOf(app *shpyrdv1.App) string {
	if ws := app.Labels[shpyrdv1.LabelWorkspace]; ws != "" {
		return ws
	}
	return store.DefaultWorkspace
}

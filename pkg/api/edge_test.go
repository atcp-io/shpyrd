package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/edge"
	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/store"
)

// edgeRequest is what ingress-nginx sends to /edge/auth, with a cookie
// and/or an Authorization header.
func edgeRequest(t *testing.T, s *Server, project, mode, cookie, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/edge/auth?project="+project+"&mode="+mode, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: s.edgeCookieName(), Value: cookie})
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestEdgeAuthDecisions(t *testing.T) {
	expenses := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "expenses", Namespace: "app-expenses"}, Spec: shpyrdv1.AppSpec{Access: shpyrdv1.AccessAuthenticated}}
	s, _ := newTestServer(t, nil, []client.Object{expenses})
	s.authz.TTL = 1
	ctx := context.Background()
	if _, _, err := s.store.PutTeam(ctx, store.DefaultWorkspace, store.Team{Name: "finance", Members: []string{"joao@acme.test"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.PutTeam(ctx, store.DefaultWorkspace, store.Team{Name: "platform", Members: []string{"ops@acme.test"}, PlatformRole: shpyrdv1.RolePlatformAdmin}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AddGrant(ctx, store.DefaultWorkspace, store.Grant{Project: "expenses", Role: shpyrdv1.RoleUser, Team: "finance"}); err != nil {
		t.Fatal(err)
	}
	joao := ext.Identity{Subject: "u-joao", Email: "joao@acme.test", Name: "João", Provider: "google"}
	pedro := ext.Identity{Subject: "u-pedro", Email: "pedro@acme.test", Provider: "google"}
	joaoSID, _ := signIn(t, s, joao)
	pedroSID, _ := signIn(t, s, pedro)
	cookieFor := func(sid, project string, preview *edge.Preview) string {
		v, err := s.edgeKeys.SignCookie(edge.CookieClaims{SessionID: sid, Project: project, Preview: preview})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	// Anonymous: sign in, please; identified mode lets them through unnamed.
	if rec := edgeRequest(t, s, "expenses", "authenticated", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", rec.Code)
	}
	if rec := edgeRequest(t, s, "expenses", "identified", "", ""); rec.Code != http.StatusOK || rec.Header().Get("X-Shpyrd-User") != "" {
		t.Errorf("anonymous identified = %d %q", rec.Code, rec.Header().Get("X-Shpyrd-User"))
	}

	// João, in Finance with the user role: in, with headers and a JWT.
	rec := edgeRequest(t, s, "expenses", "authenticated", cookieFor(joaoSID, "expenses", nil), "")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Shpyrd-User") != "joao@acme.test" || rec.Header().Get("X-Shpyrd-Teams") != "everyone,finance" || rec.Header().Get("X-Shpyrd-Roles") != "user" {
		t.Fatalf("joao = %d %v", rec.Code, rec.Header())
	}
	var claims edge.Claims
	tok := strings.TrimPrefix(rec.Header().Get("Authorization"), "Bearer ")
	if err := s.edgeKeys.Verify(tok, "JWT", &claims); err != nil || claims.Audience != "expenses" || claims.Email != "joao@acme.test" || claims.Teams[1] != "finance" || claims.Preview {
		t.Errorf("jwt: %v %+v", err, claims)
	}
	// A cookie for another project does not open this one.
	if rec := edgeRequest(t, s, "expenses", "authenticated", cookieFor(joaoSID, "crm", nil), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("foreign cookie = %d", rec.Code)
	}
	// Pedro is signed in but holds no role: refused, not asked to sign in.
	if rec := edgeRequest(t, s, "expenses", "authenticated", cookieFor(pedroSID, "expenses", nil), ""); rec.Code != http.StatusForbidden {
		t.Errorf("pedro = %d", rec.Code)
	}
	// ... unless the app only identifies.
	if rec := edgeRequest(t, s, "expenses", "identified", cookieFor(pedroSID, "expenses", nil), ""); rec.Code != http.StatusOK || rec.Header().Get("X-Shpyrd-User") != "pedro@acme.test" || rec.Header().Get("X-Shpyrd-Roles") != "" {
		t.Errorf("pedro identified = %d %v", rec.Code, rec.Header())
	}
	// Suspending João switches him off at once; reactivating brings him back.
	if _, err := s.store.TouchIdentity(ctx, store.DefaultWorkspace, store.Identity{Email: "joao@acme.test", Provider: "google"}); err != nil {
		t.Fatal(err)
	}
	req0 := httptest.NewRequest("PATCH", "/api/workspace/people/joao@acme.test", strings.NewReader(`{"status":"suspended"}`))
	req0.Header.Set("Content-Type", "application/json")
	req0.Header.Set("Authorization", "Bearer "+testToken)
	rec0 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec0, req0)
	if rec0.Code != http.StatusOK || !strings.Contains(rec0.Body.String(), `"status":"suspended"`) {
		t.Fatalf("suspend = %d %s", rec0.Code, rec0.Body.String())
	}
	if rec := edgeRequest(t, s, "expenses", "authenticated", cookieFor(joaoSID, "expenses", nil), ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "suspended") {
		t.Errorf("suspended joao = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/me", "", joaoSID, ""); !strings.Contains(rec.Body.String(), `"suspended":true`) {
		t.Errorf("me while suspended = %s", rec.Body.String())
	}
	req0 = httptest.NewRequest("PATCH", "/api/workspace/people/joao@acme.test", strings.NewReader(`{"status":"active"}`))
	req0.Header.Set("Content-Type", "application/json")
	req0.Header.Set("Authorization", "Bearer "+testToken)
	rec0 = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec0, req0)
	if rec := edgeRequest(t, s, "expenses", "authenticated", cookieFor(joaoSID, "expenses", nil), ""); rec.Code != http.StatusOK {
		t.Errorf("reactivated joao = %d", rec.Code)
	}
	// The everyone team opens the app to every signed-in person.
	if _, err := s.store.AddGrant(ctx, store.DefaultWorkspace, store.Grant{Project: "expenses", Role: shpyrdv1.RoleUser, Team: store.TeamEveryone}); err != nil {
		t.Fatal(err)
	}
	s.authz.Invalidate()
	if rec := edgeRequest(t, s, "expenses", "authenticated", cookieFor(pedroSID, "expenses", nil), ""); rec.Code != http.StatusOK || rec.Header().Get("X-Shpyrd-Teams") != "everyone" {
		t.Errorf("pedro via everyone = %d %v", rec.Code, rec.Header())
	}
	if err := s.store.DeleteProjectGrants(ctx, store.DefaultWorkspace, "expenses"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AddGrant(ctx, store.DefaultWorkspace, store.Grant{Project: "expenses", Role: shpyrdv1.RoleUser, Team: "finance"}); err != nil {
		t.Fatal(err)
	}
	s.authz.Invalidate()

	// Signing out ends it.
	s.rp.sessions.delete(ctx, pedroSID)
	if rec := edgeRequest(t, s, "expenses", "authenticated", cookieFor(pedroSID, "expenses", nil), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out = %d", rec.Code)
	}
	// The admin token opens everything as the operator.
	rec = edgeRequest(t, s, "expenses", "authenticated", "", testToken)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Shpyrd-Roles") != "admin,platform-admin" {
		t.Errorf("token = %d %v", rec.Code, rec.Header())
	}
	if rec := edgeRequest(t, s, "expenses", "authenticated", "", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", rec.Code)
	}

	// Open as: Pedro (a developer) previews what Finance sees.
	if _, err := s.store.AddGrant(ctx, store.DefaultWorkspace, store.Grant{Project: "expenses", Role: shpyrdv1.RoleDeveloper, User: "pedro@acme.test"}); err != nil {
		t.Fatal(err)
	}
	rec = edgeRequest(t, s, "expenses", "authenticated", cookieFor(pedroSID, "expenses", &edge.Preview{Teams: []string{"finance"}}), "")
	if rec.Code != http.StatusUnauthorized { // his session is gone
		t.Fatalf("preview without session = %d", rec.Code)
	}
	pedroSID, pedroCSRF := signIn(t, s, pedro)
	rec = edgeRequest(t, s, "expenses", "authenticated", cookieFor(pedroSID, "expenses", &edge.Preview{Teams: []string{"finance"}}), "")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Shpyrd-Teams") != "finance" || rec.Header().Get("X-Shpyrd-Roles") != "user" {
		t.Fatalf("preview = %d %v", rec.Code, rec.Header())
	}
	claims = edge.Claims{}
	_ = s.edgeKeys.Verify(strings.TrimPrefix(rec.Header().Get("Authorization"), "Bearer "), "JWT", &claims)
	if !claims.Preview || claims.Actor == nil || claims.Actor.Email != "pedro@acme.test" {
		t.Errorf("preview claims = %+v", claims)
	}
	// Open as a team without access: refused like a real member would be.
	if rec := edgeRequest(t, s, "expenses", "authenticated", cookieFor(pedroSID, "expenses", &edge.Preview{Teams: []string{"platform"}}), ""); rec.Code != http.StatusForbidden {
		t.Errorf("preview no access = %d", rec.Code)
	}

	// The preview API mints a callback link for the app host.
	req := httptest.NewRequest("POST", "/api/projects/expenses/preview", strings.NewReader(`{"teams":["finance"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, pedroCSRF)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: pedroSID})
	prec := httptest.NewRecorder()
	s.Handler().ServeHTTP(prec, req)
	var out struct{ URL string }
	if err := json.Unmarshal(prec.Body.Bytes(), &out); err != nil || prec.Code != http.StatusOK || !strings.HasPrefix(out.URL, "https://expenses.example.test/.shpyrd/callback?code=") {
		t.Fatalf("preview api = %d %s", prec.Code, prec.Body.String())
	}
	// Redeeming it on the app host sets the app's cookie and lands on rd.
	u, _ := url.Parse(out.URL)
	creq := httptest.NewRequest("GET", u.RequestURI(), nil)
	creq.Host = "expenses.example.test"
	crec := httptest.NewRecorder()
	s.Handler().ServeHTTP(crec, creq)
	if crec.Code != http.StatusFound || crec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %s", crec.Code, crec.Header().Get("Location"))
	}
	var set *http.Cookie
	for _, ck := range crec.Result().Cookies() {
		if ck.Name == s.edgeCookieName() {
			set = ck
		}
	}
	if set == nil || !set.HttpOnly {
		t.Fatalf("no edge cookie set: %v", crec.Result().Cookies())
	}
	if rec := edgeRequest(t, s, "expenses", "authenticated", set.Value, ""); rec.Code != http.StatusOK || rec.Header().Get("X-Shpyrd-Teams") != "finance" {
		t.Errorf("cookie from callback = %d %v", rec.Code, rec.Header())
	}
	// A code is single use.
	crec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(crec2, creq)
	if crec2.Code != http.StatusBadRequest {
		t.Errorf("second redeem = %d", crec2.Code)
	}
}

func TestEdgeSigninAndStart(t *testing.T) {
	expenses := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "expenses", Namespace: "app-expenses"}, Spec: shpyrdv1.AppSpec{Access: shpyrdv1.AccessAuthenticated}}
	s, _ := newTestServer(t, nil, []client.Object{expenses})
	s.opts.Public.DashboardURL = "https://shpyrd.example.test"

	// /.shpyrd/signin on the app host bounces to the dashboard host's start.
	req := httptest.NewRequest("GET", "/.shpyrd/signin?rd=%2Freports%3Fq%3D1", nil)
	req.Host = "expenses.example.test"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || loc.Host != "shpyrd.example.test" || loc.Path != "/.shpyrd/start" || loc.Query().Get("app") != "expenses.example.test" || loc.Query().Get("rd") != "/reports?q=1" {
		t.Fatalf("signin = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Unknown host: no bounce.
	req = httptest.NewRequest("GET", "/.shpyrd/signin?rd=/", nil)
	req.Host = "nope.example.test"
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown host = %d", rec.Code)
	}

	// start without a session: to the login page, coming back here.
	req = httptest.NewRequest("GET", loc.RequestURI(), nil)
	req.Host = "shpyrd.example.test"
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/?next=%2F.shpyrd%2Fstart") {
		t.Fatalf("start anonymous = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// start with a session: a code for the app host.
	sid, _ := signIn(t, s, ext.Identity{Subject: "u1", Email: "maria@acme.test", Provider: "local"})
	req = httptest.NewRequest("GET", loc.RequestURI(), nil)
	req.Host = "shpyrd.example.test"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	cb, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || cb.Host != "expenses.example.test" || cb.Path != "/.shpyrd/callback" || cb.Query().Get("code") == "" || cb.Query().Get("rd") != "/reports?q=1" {
		t.Fatalf("start = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// The code redeems on the app host only.
	req = httptest.NewRequest("GET", cb.RequestURI(), nil)
	req.Host = "expenses.example.test"
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/reports?q=1" || len(rec.Result().Cookies()) == 0 {
		t.Fatalf("callback = %d %s cookies=%d", rec.Code, rec.Header().Get("Location"), len(rec.Result().Cookies()))
	}

	// JWKS is public.
	rec = do(t, s, "GET", "/.well-known/jwks.json", "", false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"crv":"Ed25519"`) {
		t.Errorf("jwks = %d %s", rec.Code, rec.Body.String())
	}

	// The denied page, as nginx's error backend delivers it.
	req = httptest.NewRequest("GET", "/reports", nil)
	req.Host = "expenses.example.test"
	req.Header.Set("X-Code", "403")
	req.Header.Set("X-Ingress-Name", "expenses")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "is available to") {
		t.Errorf("denied page = %d %s", rec.Code, rec.Body.String()[:min(len(rec.Body.String()), 200)])
	}
	// The launcher lists what the caller may open: in bootstrap mode Maria
	// is a platform admin, so everything.
	rec = doCookie(t, s, "GET", "/api/launcher", "", sid, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"slug":"expenses"`) || !strings.Contains(rec.Body.String(), `"role":"admin"`) {
		t.Errorf("launcher = %d %s", rec.Code, rec.Body.String())
	}
	// Once roles are enforced, a stranger sees only public apps.
	if _, _, err := s.store.PutTeam(context.Background(), store.DefaultWorkspace, store.Team{Name: "platform", Members: []string{"ops@acme.test"}, PlatformRole: shpyrdv1.RolePlatformAdmin}); err != nil {
		t.Fatal(err)
	}
	s.authz.Invalidate()
	rec = doCookie(t, s, "GET", "/api/launcher", "", sid, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("launcher enforced = %d %s", rec.Code, rec.Body.String())
	}
}

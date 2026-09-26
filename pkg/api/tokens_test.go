package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/authz"
	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/store"
)

func TestAPITokens(t *testing.T) {
	shop := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "app-shop"}}
	s, _ := newTestServer(t, nil, []client.Object{shop})
	s.authz.TTL = 1

	// Platform admin signs in and creates a developer token.
	ctx := t.Context()
	if _, _, err := s.store.PutTeam(ctx, store.DefaultWorkspace, store.Team{Name: "ops", Members: []string{"ops@example.test"}, PlatformRole: shpyrdv1.RolePlatformAdmin}); err != nil {
		t.Fatal(err)
	}
	devGrant, err := s.store.AddGrant(ctx, store.DefaultWorkspace, store.Grant{Project: "shop", Role: shpyrdv1.RoleDeveloper, User: "dev@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	// Sign-in registers the person; tokens resolve their owner through it.
	for _, email := range []string{"ops@example.test", "dev@example.test", "pedro@example.test"} {
		if _, err := s.store.TouchIdentity(ctx, store.DefaultWorkspace, store.Identity{Email: email, Provider: "local"}); err != nil {
			t.Fatal(err)
		}
	}
	s.authz.Invalidate()
	opsSID, opsCSRF := signIn(t, s, ext.Identity{Email: "ops@example.test", Provider: "local"})
	devSID, devCSRF := signIn(t, s, ext.Identity{Email: "dev@example.test", Provider: "local"})

	// Create a token as ops: no project constraint → platform-admin level.
	body := `{"name":"ci","platformRole":"platform-viewer"}`
	req := httptest.NewRequest("POST", "/api/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, opsCSRF)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: opsSID})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token = %d %s", rec.Code, rec.Body.String())
	}
	var created TokenCreateView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Token == "" || !strings.HasPrefix(created.Token, tokenPrefix) {
		t.Fatalf("token response: %+v %v", created, err)
	}
	token := created.Token

	// Use the token: is identified as ops but with platform-viewer role.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/me", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "ops@example.test") {
		t.Fatalf("use token = %d %s", rec2.Code, rec2.Body.String())
	}
	var me struct {
		Roles authz.Roles `json:"roles"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &me)
	if me.Roles.Platform != shpyrdv1.RolePlatformViewer {
		t.Errorf("token role = %q, want platform-viewer", me.Roles.Platform)
	}

	// Developer creates a project-scoped token; cannot exceed their own role.
	body2 := `{"name":"dev-ci","projectRoles":{"shop":"developer"}}`
	req3 := httptest.NewRequest("POST", "/api/tokens", strings.NewReader(body2))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set(csrfHeader, devCSRF)
	req3.AddCookie(&http.Cookie{Name: sessionCookie, Value: devSID})
	rec3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusCreated {
		t.Fatalf("dev token = %d %s", rec3.Code, rec3.Body.String())
	}
	var devToken TokenCreateView
	_ = json.Unmarshal(rec3.Body.Bytes(), &devToken)

	// Developer cannot create an admin token.
	req4 := httptest.NewRequest("POST", "/api/tokens", strings.NewReader(`{"name":"x","projectRoles":{"shop":"admin"}}`))
	req4.Header.Set("Content-Type", "application/json")
	req4.Header.Set(csrfHeader, devCSRF)
	req4.AddCookie(&http.Cookie{Name: sessionCookie, Value: devSID})
	rec4 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusBadRequest {
		t.Errorf("escalation = %d", rec4.Code)
	}

	// List tokens.
	listRec := doCookie(t, s, "GET", "/api/tokens", "", opsSID, "")
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), "ci") {
		t.Errorf("list = %d %s", listRec.Code, listRec.Body.String())
	}

	// Revoke.
	delReq := httptest.NewRequest("DELETE", "/api/tokens/"+created.ID, nil)
	delReq.Header.Set(csrfHeader, opsCSRF)
	delReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: opsSID})
	delRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Errorf("revoke = %d", delRec.Code)
	}

	// Revoked token no longer works.
	rec5 := httptest.NewRecorder()
	req5 := httptest.NewRequest("GET", "/api/me", nil)
	req5.Header.Set("Authorization", "Bearer "+token)
	s.Handler().ServeHTTP(rec5, req5)
	if rec5.Code != http.StatusUnauthorized {
		t.Errorf("revoked token = %d", rec5.Code)
	}

	// The developer's token carries the developer role on shop...
	if got := rolesWithToken(t, s, devToken.Token); got.Projects["shop"] != shpyrdv1.RoleDeveloper {
		t.Errorf("dev token roles = %+v, want developer on shop", got)
	}
	// ...until the owner is demoted: the token follows, without a new token.
	if err := s.store.DeleteGrant(ctx, store.DefaultWorkspace, devGrant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AddGrant(ctx, store.DefaultWorkspace, store.Grant{Project: "shop", Role: shpyrdv1.RoleViewer, User: "dev@example.test"}); err != nil {
		t.Fatal(err)
	}
	s.authz.Invalidate()
	if got := rolesWithToken(t, s, devToken.Token); got.Projects["shop"] != shpyrdv1.RoleViewer {
		t.Errorf("demoted owner's token roles = %+v, want viewer on shop", got)
	}
	// A suspended owner's tokens stop working at once.
	if _, err := s.store.SetIdentityStatus(ctx, store.DefaultWorkspace, "dev@example.test", "suspended"); err != nil {
		t.Fatal(err)
	}
	s.authz.Invalidate()
	rec6 := httptest.NewRecorder()
	req6 := httptest.NewRequest("GET", "/api/me", nil)
	req6.Header.Set("Authorization", "Bearer "+devToken.Token)
	s.Handler().ServeHTTP(rec6, req6)
	if rec6.Code != http.StatusUnauthorized {
		t.Errorf("suspended owner's token = %d, want 401", rec6.Code)
	}

	// A person whose only role comes from the built-in team can still mint a
	// token at that level (the everyone team grants user on shop).
	if _, err := s.store.AddGrant(ctx, store.DefaultWorkspace, store.Grant{Project: "shop", Role: shpyrdv1.RoleUser, Team: store.TeamEveryone}); err != nil {
		t.Fatal(err)
	}
	s.authz.Invalidate()
	pedroSID, pedroCSRF := signIn(t, s, ext.Identity{Email: "pedro@example.test", Provider: "local"})
	pedroRec := doCookie(t, s, "POST", "/api/tokens", `{"name":"laptop","projectRoles":{"shop":"user"}}`, pedroSID, pedroCSRF)
	if pedroRec.Code != http.StatusCreated {
		t.Fatalf("pedro token = %d %s", pedroRec.Code, pedroRec.Body.String())
	}
	var pedroToken TokenCreateView
	_ = json.Unmarshal(pedroRec.Body.Bytes(), &pedroToken)
	if got := rolesWithToken(t, s, pedroToken.Token); got.Projects["shop"] != shpyrdv1.RoleUser {
		t.Errorf("everyone-derived token roles = %+v, want user on shop", got)
	}
	// A token cannot mint tokens, whatever its role.
	mint := httptest.NewRequest("POST", "/api/tokens", strings.NewReader(`{"name":"child","projectRoles":{"shop":"user"}}`))
	mint.Header.Set("Content-Type", "application/json")
	mint.Header.Set("Authorization", "Bearer "+pedroToken.Token)
	mintRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(mintRec, mint)
	if mintRec.Code != http.StatusForbidden {
		t.Errorf("token minting a token = %d %s", mintRec.Code, mintRec.Body.String())
	}
	// Above their level is refused with a message naming what they are.
	tooBig := doCookie(t, s, "POST", "/api/tokens", `{"name":"big","projectRoles":{"shop":"viewer"}}`, pedroSID, pedroCSRF)
	if tooBig.Code != http.StatusBadRequest || !strings.Contains(tooBig.Body.String(), "you are user") {
		t.Errorf("escalation from user = %d %s", tooBig.Code, tooBig.Body.String())
	}
}

// rolesWithToken reads /api/me with a bearer token and returns the roles.
func rolesWithToken(t *testing.T, s *Server, token string) authz.Roles {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/me with token = %d %s", rec.Code, rec.Body.String())
	}
	var me struct {
		Roles authz.Roles `json:"roles"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	return me.Roles
}

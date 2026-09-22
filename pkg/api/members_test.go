package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/ext"
)

// signIn creates a session for a user and returns its cookie and CSRF token.
func signIn(t *testing.T, s *Server, id ext.Identity) (string, string) {
	t.Helper()
	sess, err := s.rp.sessions.create(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return sess.ID, sess.CSRF
}

func TestAuthorization(t *testing.T) {
	shop := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "app-shop"}}
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, cr := newTestServer(t, nil, []client.Object{shop, blog})
	s.authz.TTL = 1 // no caching between steps

	dev := ext.Identity{Subject: "u1", Email: "dev@example.test", Name: "Dev", Provider: "local"}
	viewer := ext.Identity{Subject: "u2", Email: "viewer@example.test", Provider: "local"}
	stranger := ext.Identity{Subject: "u3", Email: "new@example.test", Provider: "local"}
	devSID, devCSRF := signIn(t, s, dev)
	viewSID, viewCSRF := signIn(t, s, viewer)
	newSID, _ := signIn(t, s, stranger)

	// Bootstrap: no teams or members yet, every signed-in user is a platform admin.
	rec := doCookie(t, s, "GET", "/api/me", "", newSID, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"platform":"platform-admin"`) || !strings.Contains(rec.Body.String(), `"enforced":false`) {
		t.Fatalf("bootstrap me: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/cluster", "", newSID, ""); rec.Code != http.StatusOK {
		t.Errorf("bootstrap cluster page: %d", rec.Code)
	}

	// The admin token creates the first team: enforcement starts.
	rec = do(t, s, "POST", "/api/teams", `{"name":"ops","members":["Ops@Example.test"],"platformRole":"platform-admin"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"members":["ops@example.test"]`) {
		t.Errorf("emails are normalised: %s", rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/teams", `{"name":"Bad Name"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("bad team name: %d", rec.Code)
	}

	// Strangers now see nothing.
	if rec := doCookie(t, s, "GET", "/api/apps/app-shop/shop", "", newSID, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "no access to project shop") {
		t.Errorf("stranger: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/cluster", "", newSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("stranger cluster: %d", rec.Code)
	}
	rec = doCookie(t, s, "GET", "/api/apps", "", newSID, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("stranger list must be empty: %d %s", rec.Code, rec.Body.String())
	}

	// Grants: a developer on shop, a viewer on shop through a team.
	if rec := do(t, s, "POST", "/api/projects/app-shop/members", `{"role":"developer","user":"dev@example.test"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("add member: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/app-shop/members", `{"role":"developer","user":"dev@example.test"}`, true); rec.Code != http.StatusConflict {
		t.Errorf("duplicate grant: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects/app-shop/members", `{"role":"owner","user":"x@example.test"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("bad role: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects/app-shop/members", `{"role":"viewer","team":"nope"}`, true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown team: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/teams", `{"name":"readers","members":["viewer@example.test"]}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("readers team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/app-shop/members", `{"role":"viewer","team":"readers"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("team grant: %d %s", rec.Code, rec.Body.String())
	}

	// Developer: sees shop only, may scale, may not destroy or manage members.
	rec = doCookie(t, s, "GET", "/api/apps", "", devSID, "")
	var list []AppSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "shop" {
		t.Errorf("developer list = %s", rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/apps/app-blog/blog", "", devSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("developer on blog: %d", rec.Code)
	}
	if rec := doCookie(t, s, "POST", "/api/apps/app-shop/shop/scale", `{"process":"web","replicas":2}`, devSID, devCSRF); rec.Code != http.StatusOK {
		t.Errorf("developer scale: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "DELETE", "/api/apps/app-shop/shop", "", devSID, devCSRF); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "role on project shop is developer") {
		t.Errorf("developer destroy: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/projects/app-shop/members", "", devSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("developer members: %d", rec.Code)
	}
	rec = doCookie(t, s, "GET", "/api/me", "", devSID, "")
	if !strings.Contains(rec.Body.String(), `"projects":{"shop":"developer"}`) || !strings.Contains(rec.Body.String(), `"enforced":true`) || !strings.Contains(rec.Body.String(), `"admin":false`) {
		t.Errorf("developer me: %s", rec.Body.String())
	}

	// Viewer (through the team): read only.
	if rec := doCookie(t, s, "GET", "/api/apps/app-shop/shop", "", viewSID, ""); rec.Code != http.StatusOK {
		t.Errorf("viewer read: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "PUT", "/api/apps/app-shop/shop/secrets", `{"set":{"A":"b"}}`, viewSID, viewCSRF); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "is viewer: it cannot change config vars") {
		t.Errorf("viewer config: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/sizes", "", viewSID, ""); rec.Code != http.StatusOK {
		t.Errorf("sizes are readable by any signed-in user: %d", rec.Code)
	}

	// Membership listing and removal by the token (platform admin).
	rec = do(t, s, "GET", "/api/projects/app-shop/members", "", true)
	var members []MemberView
	_ = json.Unmarshal(rec.Body.Bytes(), &members)
	if len(members) != 2 {
		t.Fatalf("members = %s", rec.Body.String())
	}
	for _, m := range members {
		if m.User == "dev@example.test" {
			if rec := do(t, s, "DELETE", "/api/projects/app-shop/members/"+m.Name, "", true); rec.Code != http.StatusNoContent {
				t.Errorf("remove member: %d %s", rec.Code, rec.Body.String())
			}
		}
	}
	if rec := doCookie(t, s, "GET", "/api/apps/app-shop/shop", "", devSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("developer after removal: %d", rec.Code)
	}
	// Deleting a team removes its grants.
	if rec := do(t, s, "DELETE", "/api/teams/readers", "", true); rec.Code != http.StatusNoContent {
		t.Fatalf("delete team: %d", rec.Code)
	}
	if rec := doCookie(t, s, "GET", "/api/apps/app-shop/shop", "", viewSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer after team deletion: %d", rec.Code)
	}
	var left shpyrdv1.ProjectMemberList
	_ = cr.List(context.Background(), &left)
	if len(left.Items) != 0 {
		t.Errorf("grants left: %d", len(left.Items))
	}
	if MemberName("shop", "viewer", "Ada+Dev@Example.test", "") != "shop-viewer-user-ada-plus-dev-at-example-test" {
		t.Errorf("member name = %s", MemberName("shop", "viewer", "Ada+Dev@Example.test", ""))
	}
}

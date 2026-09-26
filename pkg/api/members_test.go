package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shpyrd-io/shpyrd/pkg/kube"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/store"
)

// signIn creates a session for a user and returns its cookie and CSRF token.
func signIn(t *testing.T, s *Server, id ext.Identity) (string, string) {
	t.Helper()
	sess, err := s.rp.sessions.create(context.Background(), store.DefaultWorkspace, id, "")
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
	if rec := doCookie(t, s, "GET", "/api/projects/shop", "", newSID, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "no access to project shop") {
		t.Errorf("stranger: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/cluster", "", newSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("stranger cluster: %d", rec.Code)
	}
	rec = doCookie(t, s, "GET", "/api/projects", "", newSID, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("stranger list must be empty: %d %s", rec.Code, rec.Body.String())
	}

	// Grants: a developer on shop, a viewer on shop through a team.
	if rec := do(t, s, "POST", "/api/projects/shop/members", `{"role":"developer","user":"dev@example.test"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("add member: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/shop/members", `{"role":"developer","user":"dev@example.test"}`, true); rec.Code != http.StatusConflict {
		t.Errorf("duplicate grant: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects/shop/members", `{"role":"owner","user":"x@example.test"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("bad role: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects/shop/members", `{"role":"viewer","team":"nope"}`, true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown team: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/teams", `{"name":"readers","members":["viewer@example.test"]}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("readers team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/shop/members", `{"role":"viewer","team":"readers"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("team grant: %d %s", rec.Code, rec.Body.String())
	}

	// Developer: sees shop only, may scale, may not destroy or manage members.
	rec = doCookie(t, s, "GET", "/api/projects", "", devSID, "")
	var list []AppSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Slug != "shop" {
		t.Errorf("developer list = %s", rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/projects/blog", "", devSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("developer on blog: %d", rec.Code)
	}
	if rec := doCookie(t, s, "POST", "/api/projects/shop/scale", `{"process":"web","replicas":2}`, devSID, devCSRF); rec.Code != http.StatusOK {
		t.Errorf("developer scale: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "DELETE", "/api/projects/shop", "", devSID, devCSRF); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "role on project shop is developer") {
		t.Errorf("developer destroy: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/projects/shop/members", "", devSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("developer members: %d", rec.Code)
	}
	// Drains: developers may see a project's drains but not add them; cluster drains are platform-only (RFC-0023).
	if rec := doCookie(t, s, "GET", "/api/projects/shop/drains", "", devSID, ""); rec.Code != http.StatusOK {
		t.Errorf("developer list drains: %d", rec.Code)
	}
	if rec := doCookie(t, s, "POST", "/api/projects/shop/drains", `{"url":"https://x.example.com/"}`, devSID, devCSRF); rec.Code != http.StatusForbidden {
		t.Errorf("developer add drain: %d", rec.Code)
	}
	if rec := doCookie(t, s, "GET", "/api/drains", "", devSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("developer cluster drains: %d", rec.Code)
	}
	// Global config vars are a platform matter (RFC-0016).
	if rec := doCookie(t, s, "GET", "/api/globals", "", devSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("developer globals: %d", rec.Code)
	}
	if rec := doCookie(t, s, "PUT", "/api/globals", `{"set":{"X":"1"}}`, devSID, devCSRF); rec.Code != http.StatusForbidden {
		t.Errorf("developer set globals: %d", rec.Code)
	}
	rec = doCookie(t, s, "GET", "/api/me", "", devSID, "")
	if !strings.Contains(rec.Body.String(), `"projects":{"shop":"developer"}`) || !strings.Contains(rec.Body.String(), `"enforced":true`) || !strings.Contains(rec.Body.String(), `"admin":false`) {
		t.Errorf("developer me: %s", rec.Body.String())
	}

	// Viewer (through the team): read only.
	if rec := doCookie(t, s, "GET", "/api/projects/shop", "", viewSID, ""); rec.Code != http.StatusOK {
		t.Errorf("viewer read: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "PUT", "/api/projects/shop/secrets", `{"set":{"A":"b"}}`, viewSID, viewCSRF); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "is viewer: it cannot change config vars") {
		t.Errorf("viewer config: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/sizes", "", viewSID, ""); rec.Code != http.StatusOK {
		t.Errorf("sizes are readable by any signed-in user: %d", rec.Code)
	}

	// Membership listing and removal by the token (platform admin).
	rec = do(t, s, "GET", "/api/projects/shop/members", "", true)
	var members []MemberView
	_ = json.Unmarshal(rec.Body.Bytes(), &members)
	if len(members) != 2 {
		t.Fatalf("members = %s", rec.Body.String())
	}
	for _, m := range members {
		if m.User == "dev@example.test" {
			if rec := do(t, s, "DELETE", "/api/projects/shop/members/"+m.Name, "", true); rec.Code != http.StatusNoContent {
				t.Errorf("remove member: %d %s", rec.Code, rec.Body.String())
			}
		}
	}
	if rec := doCookie(t, s, "GET", "/api/projects/shop", "", devSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("developer after removal: %d", rec.Code)
	}
	// Deleting a team removes its grants.
	if rec := do(t, s, "DELETE", "/api/teams/readers", "", true); rec.Code != http.StatusNoContent {
		t.Fatalf("delete team: %d", rec.Code)
	}
	if rec := doCookie(t, s, "GET", "/api/projects/shop", "", viewSID, ""); rec.Code != http.StatusForbidden {
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

// adminOnlyExt mounts one route on the admin group, the way auth-local
// mounts its users API.
type adminOnlyExt struct{}

func (adminOnlyExt) Name() string                          { return "admin-only" }
func (adminOnlyExt) Description() string                   { return "test" }
func (adminOnlyExt) Components() []ext.ComponentRef        { return nil }
func (adminOnlyExt) Register(ctrl.Manager, ext.Deps) error { return nil }
func (adminOnlyExt) Types() []ext.ResourceType             { return nil }
func (adminOnlyExt) CLI(ext.CLIGlobals) []*cobra.Command   { return nil }
func (adminOnlyExt) Routes(r ext.Router, _ ext.Deps) error {
	r.Admin().GET("/admin-only", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return nil
}

// RFC-0007/RFC-0008: extension routes on the admin group (the users API)
// need the cluster.admin action, not just a session.
func TestExtensionAdminRoutesNeedPlatformAdmin(t *testing.T) {
	shop := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "app-shop"}}
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	cr := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(shop).WithStatusSubresource(&shpyrdv1.App{}).Build()
	k := &kube.Client{Kube: kubefake.NewSimpleClientset(), Namespace: "shpyrd-system"}
	s, err := newServer(k, Options{Token: testToken, Apps: cr, Extensions: []ext.Extension{adminOnlyExt{}}, Public: PublicConfig{Domain: "example.test"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.authz.TTL = 1

	// Enforcement on: one platform-admin team.
	if rec := do(t, s, "POST", "/api/teams", `{"name":"ops","members":["ops@example.test"],"platformRole":"platform-admin"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/shop/members", `{"role":"developer","user":"dev@example.test"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("add member: %d %s", rec.Code, rec.Body.String())
	}
	devSID, _ := signIn(t, s, ext.Identity{Subject: "u1", Email: "dev@example.test", Provider: "local"})
	opsSID, _ := signIn(t, s, ext.Identity{Subject: "u2", Email: "ops@example.test", Provider: "local"})

	if rec := doCookie(t, s, "GET", "/api/admin-only", "", devSID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("developer on an admin route: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/admin-only", "", opsSID, ""); rec.Code != http.StatusOK {
		t.Errorf("platform admin on an admin route: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/api/admin-only", "", true); rec.Code != http.StatusOK {
		t.Errorf("admin token on an admin route: %d", rec.Code)
	}
}

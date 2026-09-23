package api

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"shpyrd/pkg/ext"
)

// fakeIssuer is a minimal OpenID Connect provider: discovery, JWKS, an
// authorize endpoint that immediately redirects back with a code, and a
// token endpoint returning a signed id_token.
type fakeIssuer struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	nonce string
	codes map[string]bool
	// pkce records the code_challenge sent to /auth to check /token's verifier.
	challenge string
	verifier  string
	// passwords accepted by the password grant; endSession publishes an
	// end_session_endpoint in discovery (Dex does not, corporate IdPs do).
	passwords        map[string]string
	passwordAttempts int
	endSession       bool
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIssuer{key: key, codes: map[string]bool{}, passwords: map[string]string{"ada@example.test": "correct-horse"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		doc := map[string]any{
			"issuer": fi.srv.URL, "authorization_endpoint": fi.srv.URL + "/auth", "token_endpoint": fi.srv.URL + "/token",
			"jwks_uri": fi.srv.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if fi.endSession {
			doc["end_session_endpoint"] = fi.srv.URL + "/logout"
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pub := key.Public().(*rsa.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		fi.nonce = q.Get("nonce")
		fi.challenge = q.Get("code_challenge")
		if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "shpyrd" {
			http.Error(w, "bad request", 400)
			return
		}
		code := "code-" + q.Get("state")
		fi.codes[code] = true
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		if u, p, ok := r.BasicAuth(); !ok || u != "shpyrd" || p != "sekret" {
			http.Error(w, `{"error":"invalid_client"}`, 401)
			return
		}
		if r.Form.Get("grant_type") == "password" {
			// Dex's password grant: 401 access_denied on wrong credentials,
			// nonce echoed into the id_token when given.
			fi.passwordAttempts++
			user := strings.ToLower(r.Form.Get("username"))
			if fi.passwords[user] == "" || fi.passwords[user] != r.Form.Get("password") {
				http.Error(w, `{"error":"access_denied","error_description":"Invalid username or password"}`, 401)
				return
			}
			if !strings.Contains(r.Form.Get("scope"), "openid") {
				http.Error(w, `{"error":"invalid_request","error_description":"missing openid scope"}`, 400)
				return
			}
			now := time.Now()
			claims := map[string]any{
				"iss": fi.srv.URL, "sub": "user-1", "aud": "shpyrd", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
				"nonce": r.Form.Get("nonce"), "email": "Ada@Example.test", "name": "Ada Lovelace", "groups": []string{"dev"},
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": fi.sign(t, claims)})
			return
		}
		code := r.Form.Get("code")
		if !fi.codes[code] {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		delete(fi.codes, code)
		fi.verifier = r.Form.Get("code_verifier")
		sum := sha256.Sum256([]byte(fi.verifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != fi.challenge {
			http.Error(w, `{"error":"invalid_grant","error_description":"pkce"}`, 400)
			return
		}
		now := time.Now()
		claims := map[string]any{
			"iss": fi.srv.URL, "sub": "user-1", "aud": "shpyrd", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
			"nonce": fi.nonce, "email": "Ada@Example.test", "name": "Ada Lovelace", "groups": []string{"dev"},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": fi.sign(t, claims)})
	})
	fi.srv = httptest.NewServer(mux)
	t.Cleanup(fi.srv.Close)
	return fi
}

func (fi *fakeIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, fi.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func cookieValue(rec *httptest.ResponseRecorder, name string) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func TestOIDCLoginFlow(t *testing.T) {
	issuer := newFakeIssuer(t)
	s, _ := newTestServer(t, nil, nil)
	if err := s.rp.AddOIDC(context.Background(), ext.OIDCProvider{ID: "test", Label: "Test login", Issuer: issuer.srv.URL, ClientID: "shpyrd", ClientSecret: "sekret"}); err != nil {
		t.Fatalf("AddOIDC: %v", err)
	}

	// The login page learns the providers from /api/config.
	rec := do(t, s, "GET", "/api/config", "", false)
	if !strings.Contains(rec.Body.String(), `"providers":[{"id":"test","label":"Test login","kind":"oidc"}]`) || !strings.Contains(rec.Body.String(), `"token":true`) {
		t.Fatalf("config = %s", rec.Body.String())
	}

	// 1. Start: redirected to the issuer with state, nonce and PKCE.
	rec = do(t, s, "GET", "/api/auth/login?provider=test&next=/cluster", "", false)
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	authURL, _ := url.Parse(rec.Header().Get("Location"))
	if !strings.HasPrefix(authURL.String(), issuer.srv.URL+"/auth?") || authURL.Query().Get("redirect_uri") != "/api/auth/callback" {
		t.Fatalf("authorize URL = %s", authURL)
	}

	// 2. The issuer authenticates the user and redirects back with a code.
	resp, err := http.DefaultTransport.RoundTrip(mustRequest(t, "GET", authURL.String()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	if back.Path != "/api/auth/callback" || back.Query().Get("code") == "" {
		t.Fatalf("issuer redirect = %s", back)
	}

	// 3. Callback: session cookies and redirect to the requested page.
	rec = do(t, s, "GET", back.String(), "", false)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/cluster" {
		t.Fatalf("callback: %d %s -> %s", rec.Code, rec.Body.String(), rec.Header().Get("Location"))
	}
	sid, csrf := cookieValue(rec, sessionCookie), cookieValue(rec, csrfCookie)
	if sid == "" || csrf == "" {
		t.Fatalf("cookies not set: %v", rec.Result().Cookies())
	}
	if issuer.verifier == "" {
		t.Error("token exchange must carry the PKCE verifier")
	}

	// 4. The session authenticates requests; identity is normalised.
	me := doCookie(t, s, "GET", "/api/me", "", sid, "")
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"email":"ada@example.test"`) || !strings.Contains(me.Body.String(), `"provider":"test"`) {
		t.Fatalf("me: %d %s", me.Code, me.Body.String())
	}
	// Mutations need the CSRF header when authenticated by cookie.
	if rec := doCookie(t, s, "POST", "/api/projects", `{"name":"x"}`, sid, ""); rec.Code != http.StatusForbidden {
		t.Errorf("mutation without CSRF: %d", rec.Code)
	}
	if rec := doCookie(t, s, "POST", "/api/projects", `{"name":"csrf-ok"}`, sid, csrf); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Errorf("mutation with CSRF: %d %s", rec.Code, rec.Body.String())
	}
	// Replaying the code fails.
	if rec := do(t, s, "GET", back.String(), "", false); rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "login_error=") {
		t.Errorf("replayed callback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// The admin token still works and identifies itself.
	if rec := do(t, s, "GET", "/api/me", "", true); !strings.Contains(rec.Body.String(), `"provider":"token"`) {
		t.Errorf("token identity: %s", rec.Body.String())
	}

	// 5. Logout ends the session; without an end_session_endpoint the page
	// goes back to the root.
	if rec := doCookie(t, s, "POST", "/api/auth/logout", "", sid, csrf); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"redirect":"/"`) {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doCookie(t, s, "GET", "/api/me", "", sid, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("after logout: %d", rec.Code)
	}
	if s.rp.sessions.count() != 0 {
		t.Error("session should be gone")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"": "/", "/cluster": "/cluster", "//evil.test": "/", "https://evil.test": "/", "/api/x": "/", "cluster": "/", "/projects/x?tab=logs": "/projects/x?tab=logs",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func mustRequest(t *testing.T, method, u string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func doCookie(t *testing.T, s *Server, method, path, body, sid, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	if csrf != "" {
		req.Header.Set(csrfHeader, csrf)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(3)
	now := time.Now()
	rl.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !rl.allow("a") {
			t.Fatalf("attempt %d should pass", i)
		}
	}
	if rl.allow("a") {
		t.Error("burst exhausted")
	}
	if !rl.allow("b") {
		t.Error("other clients are independent")
	}
	now = now.Add(time.Minute)
	if !rl.allow("a") {
		t.Error("tokens refill over time")
	}
}

func TestTokenDisabledAndTickets(t *testing.T) {
	// A disabled token is refused with an explanation while sessions and
	// tickets keep working; /api/config stops advertising it.
	s, _ := newTestServer(t, nil, nil)
	s.opts.TokenDisabled = true
	s.opts.Public.Auth = AuthConfig{}
	if rec := do(t, s, "GET", "/api/projects", "", true); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "admin token is disabled") {
		t.Errorf("disabled token: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/api/config", "", false); !strings.Contains(rec.Body.String(), `"token":false`) {
		t.Errorf("config must not advertise the token: %s", rec.Body.String())
	}
	sid, _ := signIn(t, s, ext.Identity{Subject: "u", Email: "ada@example.test", Provider: "local"})
	if rec := doCookie(t, s, "GET", "/api/me", "", sid, ""); rec.Code != http.StatusOK {
		t.Errorf("sessions still work: %d", rec.Code)
	}

	// One-time login ticket minted by the CLI (Secret in the system namespace).
	code, err := MintLoginTicket(context.Background(), s.kube.Kube, "shpyrd-system", "patrick@laptop")
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, s, "GET", "/api/auth/ticket?code="+code+"&next=/cluster", "", false)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/cluster" || cookieValue(rec, sessionCookie) == "" {
		t.Fatalf("ticket login: %d -> %s cookies=%v", rec.Code, rec.Header().Get("Location"), rec.Result().Cookies())
	}
	me := doCookie(t, s, "GET", "/api/me", "", cookieValue(rec, sessionCookie), "")
	if !strings.Contains(me.Body.String(), `"provider":"kubeconfig"`) || !strings.Contains(me.Body.String(), `"platform":"platform-admin"`) || !strings.Contains(me.Body.String(), "patrick@laptop") {
		t.Errorf("ticket identity: %s", me.Body.String())
	}
	// Tickets are one-shot, and unknown codes are refused.
	if rec := do(t, s, "GET", "/api/auth/ticket?code="+code, "", false); !strings.Contains(rec.Header().Get("Location"), "login_error=") {
		t.Errorf("replayed ticket: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(t, s, "GET", "/api/auth/ticket?code=nope", "", false); !strings.Contains(rec.Header().Get("Location"), "login_error=") {
		t.Errorf("bogus ticket: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// An expired ticket is refused and cleaned up.
	code2, _ := MintLoginTicket(context.Background(), s.kube.Kube, "shpyrd-system", "late@laptop")
	list, _ := s.kube.Kube.CoreV1().Secrets("shpyrd-system").List(context.Background(), metav1.ListOptions{LabelSelector: LoginTicketLabel + "=true"})
	for i := range list.Items {
		list.Items[i].Data["expires"] = []byte(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
		_, _ = s.kube.Kube.CoreV1().Secrets("shpyrd-system").Update(context.Background(), &list.Items[i], metav1.UpdateOptions{})
	}
	if rec := do(t, s, "GET", "/api/auth/ticket?code="+code2, "", false); !strings.Contains(rec.Header().Get("Location"), "expired") {
		t.Errorf("expired ticket: %s", rec.Header().Get("Location"))
	}
	left, _ := s.kube.Kube.CoreV1().Secrets("shpyrd-system").List(context.Background(), metav1.ListOptions{LabelSelector: LoginTicketLabel + "=true"})
	if len(left.Items) != 0 {
		t.Errorf("stale tickets should be removed, %d left", len(left.Items))
	}
}

func TestFailedTokenAttemptsThrottled(t *testing.T) {
	s, _ := newTestServer(t, nil, nil)
	s.tokenFailures = newRateLimiter(3)
	bad := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/projects", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < 3; i++ {
		if rec := bad(); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	if rec := bad(); rec.Code != http.StatusTooManyRequests {
		t.Errorf("fourth wrong attempt should be throttled: %d", rec.Code)
	}
	// Even the right token is refused while throttled.
	if rec := do(t, s, "GET", "/api/projects", "", true); rec.Code != http.StatusTooManyRequests {
		t.Errorf("right token while throttled: %d", rec.Code)
	}
	// Each failure left an audit event.
	events, _ := s.kube.Kube.CoreV1().Events("shpyrd-system").List(context.Background(), metav1.ListOptions{})
	failed := 0
	for _, ev := range events.Items {
		if ev.Annotations["shpyrd.io/action"] == "auth.token_failed" {
			failed++
		}
	}
	if failed != 3 {
		t.Errorf("audited failures = %d, want 3", failed)
	}
	// Sessions are not affected by token throttling.
	sid, _ := signIn(t, s, ext.Identity{Subject: "u", Email: "ada@example.test", Provider: "local"})
	if rec := doCookie(t, s, "GET", "/api/me", "", sid, ""); rec.Code != http.StatusOK {
		t.Errorf("session while token throttled: %d", rec.Code)
	}
}

// RFC-0012: email and password on shpyrd's own page go through the issuer's
// password grant; the provider becomes a form, not a button.
func TestPasswordSignIn(t *testing.T) {
	issuer := newFakeIssuer(t)
	s, _ := newTestServer(t, nil, nil)
	if err := s.rp.AddOIDC(context.Background(), ext.OIDCProvider{ID: "local", Label: "Email and password", Issuer: issuer.srv.URL, ClientID: "shpyrd", ClientSecret: "sekret", Password: true}); err != nil {
		t.Fatalf("AddOIDC: %v", err)
	}
	if err := s.rp.AddOIDC(context.Background(), ext.OIDCProvider{ID: "okta", Label: "Okta", Issuer: issuer.srv.URL, ClientID: "shpyrd", ClientSecret: "sekret"}); err != nil {
		t.Fatalf("AddOIDC: %v", err)
	}

	// /api/config: the password provider is advertised as the form, the
	// other as a button.
	rec := do(t, s, "GET", "/api/config", "", false)
	if !strings.Contains(rec.Body.String(), `"providers":[{"id":"okta","label":"Okta","kind":"oidc"}]`) || !strings.Contains(rec.Body.String(), `"password":{"id":"local","label":"Email and password"}`) {
		t.Fatalf("config = %s", rec.Body.String())
	}

	// Wrong password: 401 with a message, audited by email, no cookies.
	rec = do(t, s, "POST", "/api/auth/password", `{"email":"Ada@Example.test","password":"nope"}`, false)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "wrong email or password") || cookieValue(rec, sessionCookie) != "" {
		t.Fatalf("wrong password: %d %s", rec.Code, rec.Body.String())
	}
	// Missing fields: 400.
	if rec := do(t, s, "POST", "/api/auth/password", `{"email":"ada@example.test"}`, false); rec.Code != http.StatusBadRequest {
		t.Errorf("missing password: %d", rec.Code)
	}

	// Right password: session cookies, identity normalised, next honoured.
	rec = do(t, s, "POST", "/api/auth/password", `{"email":"Ada@Example.test","password":"correct-horse","next":"/projects/shop"}`, false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next":"/projects/shop"`) {
		t.Fatalf("sign in: %d %s", rec.Code, rec.Body.String())
	}
	sid, csrf := cookieValue(rec, sessionCookie), cookieValue(rec, csrfCookie)
	if sid == "" || csrf == "" {
		t.Fatalf("cookies not set: %v", rec.Result().Cookies())
	}
	me := doCookie(t, s, "GET", "/api/me", "", sid, "")
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"email":"ada@example.test"`) || !strings.Contains(me.Body.String(), `"provider":"local"`) {
		t.Fatalf("me: %d %s", me.Code, me.Body.String())
	}
	// An unsafe next falls back to the root.
	rec = do(t, s, "POST", "/api/auth/password", `{"email":"ada@example.test","password":"correct-horse","next":"https://evil.test/"}`, false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next":"/"`) {
		t.Errorf("unsafe next: %d %s", rec.Code, rec.Body.String())
	}
	// Dex has no end_session_endpoint: no id_token is kept and logout goes
	// to the root.
	if sess, _ := s.rp.sessions.get(sid); sess.IDToken != "" {
		t.Error("id_token must not be kept for an issuer without end_session_endpoint")
	}
	if rec := doCookie(t, s, "POST", "/api/auth/logout", "", sid, csrf); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"redirect":"/"`) {
		t.Errorf("logout: %d %s", rec.Code, rec.Body.String())
	}

	// Audit: one failure by email, successes by email.
	evs, _ := s.kube.Kube.CoreV1().Events("shpyrd-system").List(context.Background(), metav1.ListOptions{})
	var failed, ok int
	for _, ev := range evs.Items {
		switch ev.Annotations["shpyrd.io/action"] {
		case "auth.login_failed":
			if ev.Annotations["shpyrd.io/target"] == "ada@example.test" {
				failed++
			}
		case "auth.login":
			if ev.Annotations["shpyrd.io/target"] == "ada@example.test" && strings.Contains(ev.Annotations["shpyrd.io/detail"], "password") {
				ok++
			}
		}
	}
	if failed != 1 || ok != 2 {
		t.Errorf("audit: %d failures, %d sign-ins", failed, ok)
	}
}

// Wrong passwords for one account are throttled before they reach the
// issuer; other accounts are unaffected.
func TestPasswordFailuresThrottled(t *testing.T) {
	issuer := newFakeIssuer(t)
	s, _ := newTestServer(t, nil, nil)
	if err := s.rp.AddOIDC(context.Background(), ext.OIDCProvider{ID: "local", Label: "Email and password", Issuer: issuer.srv.URL, ClientID: "shpyrd", ClientSecret: "sekret", Password: true}); err != nil {
		t.Fatal(err)
	}
	s.passwordFailures = newRateLimiter(3)
	for i := 0; i < 3; i++ {
		if rec := do(t, s, "POST", "/api/auth/password", `{"email":"ada@example.test","password":"nope"}`, false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	rec := do(t, s, "POST", "/api/auth/password", `{"email":"ADA@example.test","password":"correct-horse"}`, false)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("4th attempt must be throttled: %d %s", rec.Code, rec.Body.String())
	}
	if issuer.passwordAttempts != 3 {
		t.Errorf("issuer saw %d attempts, want 3 (the throttled one must not reach it)", issuer.passwordAttempts)
	}
	if rec := do(t, s, "POST", "/api/auth/password", `{"email":"grace@example.test","password":"x"}`, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("other account: %d", rec.Code)
	}
}

// Issuers that publish end_session_endpoint get RP-initiated logout: the
// id_token is kept for the hint and logout points the page at the issuer.
func TestLogoutAtIssuer(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.endSession = true
	s, _ := newTestServer(t, nil, nil)
	s.rp.baseURL = "https://shpyrd.example.test"
	if err := s.rp.AddOIDC(context.Background(), ext.OIDCProvider{ID: "local", Label: "Email and password", Issuer: issuer.srv.URL, ClientID: "shpyrd", ClientSecret: "sekret", Password: true}); err != nil {
		t.Fatal(err)
	}
	rec := do(t, s, "POST", "/api/auth/password", `{"email":"ada@example.test","password":"correct-horse"}`, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in: %d %s", rec.Code, rec.Body.String())
	}
	sid, csrf := cookieValue(rec, sessionCookie), cookieValue(rec, csrfCookie)
	sess, _ := s.rp.sessions.get(sid)
	if sess == nil || sess.IDToken == "" {
		t.Fatal("id_token must be kept for an issuer with end_session_endpoint")
	}
	rec = doCookie(t, s, "POST", "/api/auth/logout", "", sid, csrf)
	var out struct{ Redirect string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	u, err := url.Parse(out.Redirect)
	if err != nil || !strings.HasPrefix(out.Redirect, issuer.srv.URL+"/logout?") {
		t.Fatalf("logout redirect = %q", out.Redirect)
	}
	if u.Query().Get("id_token_hint") != sess.IDToken || u.Query().Get("post_logout_redirect_uri") != "https://shpyrd.example.test/" {
		t.Errorf("end session query = %v", u.Query())
	}
	if _, ok := s.rp.sessions.get(sid); ok {
		t.Error("session should be gone")
	}
}

// RFC-0058: a provider bound to a Dex connector sends connector_id so Dex
// skips its chooser; its kind reaches the page for the icon.
func TestConnectorProvider(t *testing.T) {
	issuer := newFakeIssuer(t)
	s, _ := newTestServer(t, nil, nil)
	if err := s.rp.AddOIDC(context.Background(), ext.OIDCProvider{ID: "github", Label: "GitHub", Kind: "github", ConnectorID: "github", Issuer: issuer.srv.URL, ClientID: "shpyrd", ClientSecret: "sekret"}); err != nil {
		t.Fatal(err)
	}
	rec := do(t, s, "GET", "/api/config", "", false)
	if !strings.Contains(rec.Body.String(), `{"id":"github","label":"GitHub","kind":"github"}`) {
		t.Fatalf("config = %s", rec.Body.String())
	}
	rec = do(t, s, "GET", "/api/auth/login?provider=github", "", false)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || loc.Query().Get("connector_id") != "github" {
		t.Fatalf("authorize URL = %d %s", rec.Code, loc)
	}
}

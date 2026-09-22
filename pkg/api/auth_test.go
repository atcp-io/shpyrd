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
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIssuer{key: key, codes: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": fi.srv.URL, "authorization_endpoint": fi.srv.URL + "/auth", "token_endpoint": fi.srv.URL + "/token",
			"jwks_uri": fi.srv.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
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
		if u, p, ok := r.BasicAuth(); !ok || u != "shpyrd" || p != "sekret" {
			http.Error(w, `{"error":"invalid_client"}`, 401)
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
	if !strings.Contains(rec.Body.String(), `"providers":[{"id":"test","label":"Test login"}]`) || !strings.Contains(rec.Body.String(), `"token":true`) {
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
	if rec := doCookie(t, s, "POST", "/api/apps", `{"name":"x"}`, sid, ""); rec.Code != http.StatusForbidden {
		t.Errorf("mutation without CSRF: %d", rec.Code)
	}
	if rec := doCookie(t, s, "POST", "/api/apps", `{"name":"csrf-ok"}`, sid, csrf); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
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

	// 5. Logout ends the session.
	if rec := doCookie(t, s, "POST", "/api/auth/logout", "", sid, csrf); rec.Code != http.StatusNoContent {
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
		"": "/", "/cluster": "/cluster", "//evil.test": "/", "https://evil.test": "/", "/api/x": "/", "cluster": "/", "/apps/app-x/x?tab=logs": "/apps/app-x/x?tab=logs",
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

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A tiny issuer: one Ed25519 key, a JWKS endpoint and a signer, shaped like
// shpyrd's edge.
type fakeIssuer struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	srv  *httptest.Server
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIssuer{pub: pub, priv: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(pub),
		}}})
	})
	fi.srv = httptest.NewServer(mux)
	t.Cleanup(fi.srv.Close)
	return fi
}

func (fi *fakeIssuer) token(t *testing.T, claims map[string]any, kid string, key ed25519.PrivateKey) string {
	h, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kid})
	p, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	sig := ed25519.Sign(key, []byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerify(t *testing.T) {
	fi := newFakeIssuer(t)
	v := &verifier{issuer: fi.srv.URL, audience: "hello"}
	now := time.Now().Unix()
	good := map[string]any{"iss": fi.srv.URL, "aud": "hello", "sub": "u1", "email": "ada@example.test", "teams": []string{"eng"}, "roles": []string{"user"}, "iat": now, "exp": now + 300}
	req := func(tok string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		return r
	}
	who, err := v.verify(req(fi.token(t, good, "k1", fi.priv)))
	if err != nil || who.Email != "ada@example.test" || len(who.Teams) != 1 {
		t.Fatalf("good token: %+v %v", who, err)
	}

	// Another key, same kid: the signature must fail.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := v.verify(req(fi.token(t, good, "k1", otherPriv))); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("forged token accepted: %v", err)
	}
	// Wrong audience: another project's token.
	bad := map[string]any{}
	for k, val := range good {
		bad[k] = val
	}
	bad["aud"] = "shop"
	if _, err := v.verify(req(fi.token(t, bad, "k1", fi.priv))); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Errorf("other project's token accepted: %v", err)
	}
	// Wrong issuer.
	bad["aud"], bad["iss"] = "hello", "https://evil.example"
	if _, err := v.verify(req(fi.token(t, bad, "k1", fi.priv))); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Errorf("other issuer's token accepted: %v", err)
	}
	// Expired.
	bad["iss"], bad["exp"] = fi.srv.URL, now-600
	if _, err := v.verify(req(fi.token(t, bad, "k1", fi.priv))); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired token accepted: %v", err)
	}
	// No token, garbage, wrong alg.
	if _, err := v.verify(req("")); err == nil {
		t.Error("missing token accepted")
	}
	if _, err := v.verify(req("a.b")); err == nil {
		t.Error("garbage accepted")
	}
	hs, _ := json.Marshal(map[string]string{"alg": "HS256", "kid": "k1"})
	if _, err := v.verify(req(base64.RawURLEncoding.EncodeToString(hs) + ".e30.AAAA")); err == nil || !strings.Contains(err.Error(), "alg") {
		t.Errorf("HS256 accepted: %v", err)
	}
}

package edge

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/shpyrd-io/shpyrd/pkg/store"
)

func TestKeysRoundTrip(t *testing.T) {
	kube := kubefake.NewSimpleClientset()
	k1, err := LoadOrCreateKeys(context.Background(), kube, "shpyrd-system")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreateKeys(context.Background(), kube, "shpyrd-system")
	if err != nil || k2.KID != k1.KID || !k2.Public.Equal(k1.Public) {
		t.Fatalf("second load must return the same key: %v", err)
	}
	jwks := k1.JWKS()
	keys := jwks["keys"].([]map[string]any)
	if len(keys) != 1 || keys[0]["kid"] != k1.KID || keys[0]["crv"] != "Ed25519" {
		t.Errorf("jwks = %v", jwks)
	}
	x, _ := base64.RawURLEncoding.DecodeString(keys[0]["x"].(string))
	if !k1.Public.Equal(ed25519PublicKey(x)) {
		t.Error("jwks x is not the public key")
	}

	claims := Claims{Issuer: "https://shpyrd.example.test", Subject: "idn_1", Audience: "expenses", Email: "joao@acme.test", Roles: []string{"user"}, Teams: []string{"finance"}, ExpiresAt: time.Now().Add(time.Minute).Unix()}
	tok, err := k1.Sign("JWT", claims)
	if err != nil || strings.Count(tok, ".") != 2 {
		t.Fatalf("sign: %v %s", err, tok)
	}
	var back Claims
	if err := k1.Verify(tok, "JWT", &back); err != nil || back.Email != "joao@acme.test" || back.Audience != "expenses" {
		t.Fatalf("verify: %v %+v", err, back)
	}
	// Wrong type, tampered body, other key.
	if err := k1.Verify(tok, "shpyrd-edge", &back); err == nil {
		t.Error("type must be checked")
	}
	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"aud":"other"}`)) + "." + parts[2]
	if err := k1.Verify(tampered, "JWT", &back); err == nil {
		t.Error("tampered body must fail")
	}
	other, _ := GenerateKeys()
	if err := other.Verify(tok, "JWT", &back); err == nil {
		t.Error("another key must fail")
	}
}

func TestCookieAndCodes(t *testing.T) {
	k, _ := GenerateKeys()
	val, err := k.SignCookie(CookieClaims{SessionID: "s1", Project: "expenses"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := k.VerifyCookie(val, "expenses")
	if err != nil || c.SessionID != "s1" || c.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("cookie: %v %+v", err, c)
	}
	if _, err := k.VerifyCookie(val, "crm"); err == nil {
		t.Error("a cookie opens one project only")
	}
	expired, _ := k.SignCookie(CookieClaims{SessionID: "s1", Project: "expenses", ExpiresAt: time.Now().Add(-time.Second).Unix()})
	if _, err := k.VerifyCookie(expired, "expenses"); err == nil {
		t.Error("expired cookie must fail")
	}

	ctx := context.Background()
	mem := store.NewMemory()
	codes := NewCodes(mem)
	code, err := codes.Mint(ctx, "Expenses.acme.test", CookieClaims{SessionID: "s1", Project: "expenses", Preview: &Preview{Teams: []string{"finance"}}})
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := codes.Mint(ctx, "expenses.acme.test", CookieClaims{SessionID: "s1", Project: "expenses"})
	if _, err := codes.Redeem(ctx, wrong, "crm.acme.test"); err == nil {
		t.Error("code is bound to the host")
	}
	if _, err := codes.Redeem(ctx, wrong, "expenses.acme.test"); err == nil {
		t.Error("an attempt for the wrong host burns the code")
	}
	got, err := codes.Redeem(ctx, code, "expenses.acme.test")
	if err != nil || got.Preview == nil || got.Preview.Teams[0] != "finance" {
		t.Fatalf("redeem: %v %+v", err, got)
	}
	if _, err := codes.Redeem(ctx, code, "expenses.acme.test"); err == nil {
		t.Error("a code is redeemed once")
	}
	codes.now = func() time.Time { return time.Now().Add(-2 * CodeTTL) } // minted in the past: already expired
	late, _ := codes.Mint(ctx, "expenses.acme.test", CookieClaims{})
	codes.now = time.Now
	if _, err := codes.Redeem(ctx, late, "expenses.acme.test"); err == nil {
		t.Error("stale code must fail")
	}
}

func ed25519PublicKey(b []byte) ed25519.PublicKey { return ed25519.PublicKey(b) }

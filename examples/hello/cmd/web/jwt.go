// Verifying who the visitor is, without trusting headers.
//
// Behind shpyrd's edge every request carries "Authorization: Bearer <JWT>":
// an EdDSA (Ed25519) token about the visitor, signed by the platform and
// valid for five minutes. The convenience headers (X-Shpyrd-User, ...) are
// safe only because nothing but the ingress can reach the app; the JWT is
// safe anywhere. The keys are published at <issuer>/.well-known/jwks.json,
// and the platform tells the app what to expect through the environment:
//
//	SHPYRD_ISSUER    https://shpyrd.example.com   (the workspace's dashboard)
//	SHPYRD_PROJECT   hello                        (the JWT's audience)
//	SHPYRD_WORKSPACE default
//
// No library needed: the standard library has Ed25519, JSON and base64.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Visitor is what the JWT says about the caller.
type Visitor struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  string   `json:"aud"`
	Email     string   `json:"email"`
	Name      string   `json:"name"`
	Workspace string   `json:"ws"`
	Project   string   `json:"project"`
	Roles     []string `json:"roles"`
	Teams     []string `json:"teams"`
	Realm     string   `json:"realm"`
	Provider  string   `json:"provider"`
	Preview   bool     `json:"preview"`
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
}

// verifier checks JWTs against the issuer's published keys.
type verifier struct {
	issuer   string // expected iss, and where the keys live
	audience string // expected aud: this project's slug

	mu      sync.Mutex
	keys    map[string]ed25519.PublicKey // by kid
	fetched time.Time
}

func newVerifier() *verifier {
	return &verifier{issuer: strings.TrimSuffix(os.Getenv("SHPYRD_ISSUER"), "/"), audience: os.Getenv("SHPYRD_PROJECT")}
}

// configured says the platform told us what to expect.
func (v *verifier) configured() bool { return v.issuer != "" && v.audience != "" }

// verify checks the bearer token of a request and returns the visitor.
func (v *verifier) verify(r *http.Request) (*Visitor, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if raw == "" {
		return nil, errors.New("no bearer token")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if b, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil || json.Unmarshal(b, &header) != nil {
		return nil, errors.New("bad header")
	}
	if header.Alg != "EdDSA" {
		return nil, fmt.Errorf("unexpected alg %q", header.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("bad signature encoding")
	}
	key, err := v.key(header.Kid)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("signature does not verify")
	}
	var who Visitor
	if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err != nil || json.Unmarshal(b, &who) != nil {
		return nil, errors.New("bad claims")
	}
	now := time.Now().Unix()
	switch {
	case who.Issuer != v.issuer:
		return nil, fmt.Errorf("issuer %q is not %q", who.Issuer, v.issuer)
	case who.Audience != v.audience:
		return nil, fmt.Errorf("audience %q is not this project (%q)", who.Audience, v.audience)
	case who.ExpiresAt < now-30:
		return nil, errors.New("token expired")
	case who.IssuedAt > now+30:
		return nil, errors.New("token from the future")
	}
	return &who, nil
}

// key returns the public key for a kid, refreshing the JWKS when the kid
// is unknown (rotation) or the cache is older than ten minutes.
func (v *verifier) key(kid string) (ed25519.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && time.Since(v.fetched) < 10*time.Minute {
		return k, nil
	}
	if err := v.refresh(); err != nil {
		if k, ok := v.keys[kid]; ok {
			return k, nil // stale keys beat no keys
		}
		return nil, err
	}
	k, ok := v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown key %q", kid)
	}
	return k, nil
}

func (v *verifier) refresh() error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(v.issuer + "/.well-known/jwks.json")
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: %s", resp.Status)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			Kid string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			continue
		}
		keys[k.Kid] = ed25519.PublicKey(x)
	}
	if len(keys) == 0 {
		return errors.New("jwks has no Ed25519 keys")
	}
	v.keys, v.fetched = keys, time.Now()
	return nil
}

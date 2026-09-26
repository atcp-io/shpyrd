// Package edge is the identity the platform hands to apps (RFC-0033): the
// ingress asks /edge/auth who the caller is and whether they may open the
// app; the answer carries a short-lived JWT signed with the platform's key,
// which apps verify against the JWKS. The same key signs the per-app-host
// cookie the browser gets after signing in, so an app never sees the
// dashboard's session cookie — only a token bound to that one project.
package edge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/shpyrd-io/shpyrd/pkg/store"
)

// KeysSecretName holds the signing key pair (RFC-0033).
const KeysSecretName = "shpyrd-edge-keys"

// Lifetimes.
const (
	TokenTTL  = 5 * time.Minute  // the JWT an app receives
	CookieTTL = 12 * time.Hour   // the per-app-host cookie (the session is checked on every use anyway)
	CodeTTL   = 60 * time.Second // the one-time code carrying a session to an app host
)

// Keys is the platform's Ed25519 key pair with its key id.
type Keys struct {
	KID     string
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

// LoadOrCreateKeys reads the key pair from the Secret, generating it once.
func LoadOrCreateKeys(ctx context.Context, kube kubernetes.Interface, namespace string) (*Keys, error) {
	sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, KeysSecretName, metav1.GetOptions{})
	if err == nil && len(sec.Data["private"]) == ed25519.SeedSize {
		priv := ed25519.NewKeyFromSeed(sec.Data["private"])
		return &Keys{KID: string(sec.Data["kid"]), Private: priv, Public: priv.Public().(ed25519.PublicKey)}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	k, err := GenerateKeys()
	if err != nil {
		return nil, err
	}
	sec = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: KeysSecretName, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "shpyrd"}},
		Data:       map[string][]byte{"private": k.Private.Seed(), "public": k.Public, "kid": []byte(k.KID)},
	}
	if _, err := kube.CoreV1().Secrets(namespace).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) { // another replica won
			return LoadOrCreateKeys(ctx, kube, namespace)
		}
		return nil, err
	}
	return k, nil
}

// GenerateKeys makes a fresh key pair (tests, first start).
func GenerateKeys() (*Keys, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(pub)
	return &Keys{KID: hex.EncodeToString(sum[:8]), Private: priv, Public: pub}, nil
}

// JWKS is the public key set apps verify against.
func (k *Keys) JWKS() map[string]any {
	return map[string]any{"keys": []map[string]any{{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": k.KID,
		"x": base64.RawURLEncoding.EncodeToString(k.Public),
	}}}
}

// Claims is what an app receives about the caller.
type Claims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  string   `json:"aud"` // the project slug
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
	Email     string   `json:"email,omitempty"`
	Name      string   `json:"name,omitempty"`
	Workspace string   `json:"ws"`
	Project   string   `json:"project"`
	Roles     []string `json:"roles"`
	Teams     []string `json:"teams"`
	Realm     string   `json:"realm"` // workspace, console:<name>, operator
	Provider  string   `json:"provider,omitempty"`
	// Preview marks an "Open as" session; Actor is who is really there.
	Preview bool   `json:"preview,omitempty"`
	Actor   *Actor `json:"act,omitempty"`
}

// Actor is the real identity behind a preview.
type Actor struct {
	Subject string `json:"sub"`
	Email   string `json:"email,omitempty"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	KID string `json:"kid"`
}

// Sign produces a compact JWT (EdDSA) of any claims value; typ names the
// token's purpose ("JWT" for apps, "shpyrd-edge" for cookies).
func (k *Keys) Sign(typ string, claims any) (string, error) {
	h, _ := json.Marshal(header{Alg: "EdDSA", Typ: typ, KID: k.KID})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(body)
	sig := ed25519.Sign(k.Private, []byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify checks the signature and the type and decodes the claims.
func (k *Keys) Verify(token, typ string, into any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("malformed token")
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return errors.New("malformed token")
	}
	var h header
	if err := json.Unmarshal(rawHeader, &h); err != nil || h.Alg != "EdDSA" || h.Typ != typ {
		return errors.New("unexpected token header")
	}
	if h.KID != k.KID {
		return errors.New("unknown key")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(k.Public, []byte(parts[0]+"."+parts[1]), sig) {
		return errors.New("bad signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("malformed token")
	}
	return json.Unmarshal(body, into)
}

// Preview is what "Open as" substitutes: the teams the app should see, or
// nobody at all.
type Preview struct {
	Teams     []string `json:"teams,omitempty"`
	Anonymous bool     `json:"anonymous,omitempty"`
}

// CookieClaims is the per-app-host cookie: it names the dashboard session
// (checked on every request, so sign-out ends it) and the one project it
// opens; a preview cookie carries what to pretend.
type CookieClaims struct {
	SessionID string   `json:"sid"`
	Project   string   `json:"project"`
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
	Preview   *Preview `json:"preview,omitempty"`
}

const cookieTyp = "shpyrd-edge"

// SignCookie mints the cookie value.
func (k *Keys) SignCookie(c CookieClaims) (string, error) {
	now := time.Now()
	if c.IssuedAt == 0 {
		c.IssuedAt = now.Unix()
	}
	if c.ExpiresAt == 0 {
		c.ExpiresAt = now.Add(CookieTTL).Unix()
	}
	return k.Sign(cookieTyp, c)
}

// VerifyCookie checks a cookie value for the given project.
func (k *Keys) VerifyCookie(value, project string) (*CookieClaims, error) {
	var c CookieClaims
	if err := k.Verify(value, cookieTyp, &c); err != nil {
		return nil, err
	}
	if c.Project != project {
		return nil, errors.New("cookie is for another app")
	}
	if time.Now().Unix() >= c.ExpiresAt {
		return nil, errors.New("cookie expired")
	}
	return &c, nil
}

// Codes hands a session from the dashboard host to an app host: a one-time
// code minted at /.shpyrd/start and redeemed at /.shpyrd/callback within a
// minute, kept in the control-plane store so every replica can redeem it.
type Codes struct {
	store store.Sessions
	now   func() time.Time
}

// NewCodes returns a code store on top of the control-plane store.
func NewCodes(st store.Sessions) *Codes { return &Codes{store: st, now: time.Now} }

// Kinds of one-time codes: what a code may be redeemed as. A code minted
// for one purpose is worthless for another.
const (
	KindEdge    = "edge"    // dashboard host → app host: an app cookie
	KindSession = "session" // console host → workspace host: a session (RFC-0033 phase 6)
)

// envelope wraps the claims of a code with their kind.
type envelope struct {
	Kind   string          `json:"kind"`
	Claims json.RawMessage `json:"claims"`
}

// Mint stores the claims for the host and returns the code.
func (c *Codes) Mint(ctx context.Context, host string, claims CookieClaims) (string, error) {
	return c.MintJSON(ctx, host, KindEdge, claims)
}

// MintJSON stores any claims of a kind for the host and returns the code.
func (c *Codes) MintJSON(ctx context.Context, host, kind string, claims any) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	inner, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(envelope{Kind: kind, Claims: inner})
	if err != nil {
		return "", err
	}
	if err := c.store.PutCode(ctx, store.Code{Code: code, Host: strings.ToLower(host), Claims: body, ExpiresAt: c.now().Add(CodeTTL)}); err != nil {
		return "", err
	}
	return code, nil
}

// Redeem returns the claims once, for the right host. Any attempt burns
// the code.
func (c *Codes) Redeem(ctx context.Context, code, host string) (*CookieClaims, error) {
	var claims CookieClaims
	if err := c.RedeemJSON(ctx, code, host, KindEdge, &claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

// RedeemJSON returns the claims once, for the right host and kind. Any
// attempt burns the code.
func (c *Codes) RedeemJSON(ctx context.Context, code, host, kind string, into any) error {
	e, err := c.store.TakeCode(ctx, code)
	if err != nil {
		return errors.New("unknown, used or expired code")
	}
	if e.Host != strings.ToLower(host) {
		return fmt.Errorf("code is for %s", e.Host)
	}
	var env envelope
	if err := json.Unmarshal(e.Claims, &env); err != nil || env.Kind == "" {
		// Codes minted before kinds existed carry bare claims of the edge kind.
		if kind != KindEdge {
			return errors.New("code is of another kind")
		}
		return json.Unmarshal(e.Claims, into)
	}
	if env.Kind != kind {
		return errors.New("code is of another kind")
	}
	return json.Unmarshal(env.Claims, into)
}

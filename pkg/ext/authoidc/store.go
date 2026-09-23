// Package authoidc is the auth-oidc extension (RFC-0058): sign-in through
// any OpenID Connect issuer (Okta first) connected directly to the
// dashboard's relying party. Each provider lives in one Secret,
// shpyrd-oidc-<id>, so the client secret never leaves the cluster and no
// re-render of the server is needed to add one.
package authoidc

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"shpyrd/pkg/ext"
)

// Name of the extension.
const Name = "auth-oidc"

// LabelProvider marks the Secret of a provider with its id.
const LabelProvider = "shpyrd.io/oidc-provider"

// DefaultScopes beyond openid, email and profile: groups, which Okta and
// most corporate issuers accept and which feeds Teams.
var DefaultScopes = []string{"groups"}

var idRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$`)

// Provider is a configured issuer.
type Provider struct {
	ID           string
	Label        string
	Issuer       string
	ClientID     string
	ClientSecret string
	// Scopes beyond openid, email and profile.
	Scopes []string
}

// SecretName of a provider's Secret.
func SecretName(id string) string { return "shpyrd-oidc-" + id }

// ValidateID checks a provider id: it appears in URLs and the audit trail
// and never changes.
func ValidateID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid provider id %q: lowercase letters, digits and dashes (max 32 characters)", id)
	}
	if id == "local" || id == "token" || id == "kubeconfig" {
		return fmt.Errorf("provider id %q is reserved", id)
	}
	return nil
}

// Validate checks a provider before it is stored.
func (p Provider) Validate() error {
	if err := ValidateID(p.ID); err != nil {
		return err
	}
	if !strings.HasPrefix(p.Issuer, "https://") || strings.HasSuffix(p.Issuer, "/.well-known/openid-configuration") {
		return errors.New("issuer must be an https URL as printed by the identity provider (without /.well-known/...)")
	}
	if strings.TrimSpace(p.ClientID) == "" || strings.TrimSpace(p.ClientSecret) == "" {
		return errors.New("client id and client secret are required")
	}
	return nil
}

// OIDC converts the provider into what the relying party registers.
func (p Provider) OIDC() ext.OIDCProvider {
	return ext.OIDCProvider{
		ID: p.ID, Label: p.Label, Issuer: strings.TrimRight(p.Issuer, "/"),
		ClientID: p.ClientID, ClientSecret: p.ClientSecret, Scopes: p.Scopes, Kind: "oidc",
	}
}

// Store keeps providers as Secrets in the system namespace.
type Store struct {
	Kube      kubernetes.Interface
	Namespace string
}

func fromSecret(sec corev1.Secret) Provider {
	get := func(k string) string { return strings.TrimSpace(string(sec.Data[k])) }
	p := Provider{
		ID: sec.Labels[LabelProvider], Label: get("label"), Issuer: get("issuer"),
		ClientID: get("client-id"), ClientSecret: get("client-secret"),
	}
	if raw, ok := sec.Data["scopes"]; ok {
		p.Scopes = strings.Fields(string(raw))
	} else {
		p.Scopes = DefaultScopes
	}
	if p.Label == "" {
		p.Label = p.ID
	}
	return p
}

// List returns the configured providers sorted by id.
func (s *Store) List(ctx context.Context) ([]Provider, error) {
	list, err := s.Kube.CoreV1().Secrets(s.Namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelProvider})
	if err != nil {
		return nil, err
	}
	out := make([]Provider, 0, len(list.Items))
	for _, sec := range list.Items {
		if sec.Labels[LabelProvider] == "" {
			continue
		}
		out = append(out, fromSecret(sec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns one provider.
func (s *Store) Get(ctx context.Context, id string) (*Provider, error) {
	sec, err := s.Kube.CoreV1().Secrets(s.Namespace).Get(ctx, SecretName(id), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	p := fromSecret(*sec)
	return &p, nil
}

// Set creates or replaces a provider. It reports whether it existed.
func (s *Store) Set(ctx context.Context, p Provider) (bool, error) {
	if err := p.Validate(); err != nil {
		return false, err
	}
	scopes := p.Scopes
	if scopes == nil {
		scopes = DefaultScopes
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: SecretName(p.ID), Namespace: s.Namespace,
			Labels: map[string]string{LabelProvider: p.ID, "app.kubernetes.io/managed-by": "shpyrd"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"label": []byte(p.Label), "issuer": []byte(strings.TrimRight(p.Issuer, "/")), "client-id": []byte(p.ClientID),
			"client-secret": []byte(p.ClientSecret), "scopes": []byte(strings.Join(scopes, " ")),
		},
	}
	secrets := s.Kube.CoreV1().Secrets(s.Namespace)
	if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); err == nil {
		return false, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return false, err
	}
	cur, err := secrets.Get(ctx, sec.Name, metav1.GetOptions{})
	if err != nil {
		return true, err
	}
	cur.Labels = sec.Labels
	cur.Data = sec.Data
	cur.StringData = nil
	_, err = secrets.Update(ctx, cur, metav1.UpdateOptions{})
	return true, err
}

// Remove deletes a provider; unknown ids are an error.
func (s *Store) Remove(ctx context.Context, id string) error {
	err := s.Kube.CoreV1().Secrets(s.Namespace).Delete(ctx, SecretName(id), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("no provider %q; see `shpyrd auth oidc list`", id)
	}
	return err
}

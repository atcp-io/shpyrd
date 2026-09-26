package authlocal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Dex connectors (RFC-0058): GitHub and Google sign in through Dex, which
// reads connectors from its Kubernetes storage as Connector objects, so
// adding one is an API call, not a Dex restart. The dashboard shows a
// button per connector and sends Dex straight to it (connector_id).

// ConnectorGVR is Dex's connector resource.
var ConnectorGVR = schema.GroupVersionResource{Group: "dex.coreos.com", Version: "v1", Resource: "connectors"}

// ConnectorKinds the CLI and the Workspace page know how to configure.
var ConnectorKinds = []string{"github", "google", "microsoft", "oidc"}

var connectorIDRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$`)

// Connector is a Dex connector as shown to people: no secrets.
type Connector struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
	// Detail summarises the configuration (organisation, hosted domain).
	Detail string `json:"detail,omitempty"`
}

// ConnectorSpec is what the CLI collects for a new connector.
type ConnectorSpec struct {
	Type         string
	ID           string
	Name         string
	ClientID     string
	ClientSecret string
	// Org limits GitHub sign-in to members of one organisation (and loads
	// its teams as groups); HostedDomain limits Google to one workspace.
	Org          string
	HostedDomain string
	// Tenant limits Microsoft sign-in to one Entra tenant (id or domain);
	// empty accepts any work or school account.
	Tenant string
	// Issuer is a generic OpenID Connect provider's issuer URL (Okta,
	// Keycloak, Auth0, ...).
	Issuer string
}

// microsoftConfig mirrors Dex's microsoft connector configuration.
type microsoftConfig struct {
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
	RedirectURI  string `json:"redirectURI"`
	Tenant       string `json:"tenant,omitempty"`
	// Groups arrive only with the tenant's consent; on by default.
	UseGroupsAsWhitelist bool `json:"useGroupsAsWhitelist,omitempty"`
}

// oidcConfig mirrors Dex's oidc connector configuration.
type oidcConfig struct {
	Issuer                    string   `json:"issuer"`
	ClientID                  string   `json:"clientID"`
	ClientSecret              string   `json:"clientSecret"`
	RedirectURI               string   `json:"redirectURI"`
	Scopes                    []string `json:"scopes,omitempty"`
	InsecureEnableGroups      bool     `json:"insecureEnableGroups,omitempty"`
	GetUserInfo               bool     `json:"getUserInfo,omitempty"`
	UserNameKey               string   `json:"userNameKey,omitempty"`
	InsecureSkipEmailVerified bool     `json:"insecureSkipEmailVerified,omitempty"`
}

// githubConfig mirrors Dex's github connector configuration.
type githubConfig struct {
	ClientID      string      `json:"clientID"`
	ClientSecret  string      `json:"clientSecret"`
	RedirectURI   string      `json:"redirectURI"`
	Orgs          []githubOrg `json:"orgs,omitempty"`
	LoadAllGroups bool        `json:"loadAllGroups,omitempty"`
	TeamNameField string      `json:"teamNameField,omitempty"`
}

type githubOrg struct {
	Name string `json:"name"`
}

// googleConfig mirrors Dex's google connector configuration (groups need a
// service account and stay configuration only, RFC-0058).
type googleConfig struct {
	ClientID      string   `json:"clientID"`
	ClientSecret  string   `json:"clientSecret"`
	RedirectURI   string   `json:"redirectURI"`
	HostedDomains []string `json:"hostedDomains,omitempty"`
}

// ConnectorStore manages Dex connectors in the system namespace.
type ConnectorStore struct {
	Dynamic   dynamic.Interface
	Namespace string
	// Issuer is Dex's external URL; its callback is <issuer>/callback.
	Issuer string
}

func (s *ConnectorStore) res() dynamic.ResourceInterface {
	return s.Dynamic.Resource(ConnectorGVR).Namespace(s.Namespace)
}

// Validate checks the spec and fills defaults (id and name from the type).
func (spec *ConnectorSpec) Validate() error {
	known := false
	for _, k := range ConnectorKinds {
		known = known || k == spec.Type
	}
	if !known {
		return fmt.Errorf("unknown connector type %q (known: %s)", spec.Type, strings.Join(ConnectorKinds, ", "))
	}
	if spec.ID == "" {
		spec.ID = spec.Type
	}
	if !connectorIDRe.MatchString(spec.ID) || spec.ID == "local" {
		return fmt.Errorf("invalid connector id %q: lowercase letters, digits and dashes", spec.ID)
	}
	if spec.Name == "" {
		spec.Name = map[string]string{"github": "GitHub", "google": "Google", "microsoft": "Microsoft", "oidc": "Single sign-on"}[spec.Type]
	}
	if strings.TrimSpace(spec.ClientID) == "" || strings.TrimSpace(spec.ClientSecret) == "" {
		return errors.New("client id and client secret are required")
	}
	if spec.Type == "oidc" {
		if !strings.HasPrefix(spec.Issuer, "https://") {
			return errors.New("an OpenID Connect provider needs its issuer URL (https://...)")
		}
	}
	return nil
}

func (s *ConnectorStore) config(spec ConnectorSpec) ([]byte, error) {
	redirect := strings.TrimRight(s.Issuer, "/") + "/callback"
	switch spec.Type {
	case "github":
		cfg := githubConfig{ClientID: spec.ClientID, ClientSecret: spec.ClientSecret, RedirectURI: redirect, TeamNameField: "slug"}
		if spec.Org != "" {
			cfg.Orgs = []githubOrg{{Name: spec.Org}}
		} else {
			cfg.LoadAllGroups = true
		}
		return json.Marshal(cfg)
	case "google":
		cfg := googleConfig{ClientID: spec.ClientID, ClientSecret: spec.ClientSecret, RedirectURI: redirect}
		if spec.HostedDomain != "" {
			cfg.HostedDomains = []string{spec.HostedDomain}
		}
		return json.Marshal(cfg)
	case "microsoft":
		cfg := microsoftConfig{ClientID: spec.ClientID, ClientSecret: spec.ClientSecret, RedirectURI: redirect, Tenant: spec.Tenant}
		return json.Marshal(cfg)
	case "oidc":
		cfg := oidcConfig{Issuer: strings.TrimRight(spec.Issuer, "/"), ClientID: spec.ClientID, ClientSecret: spec.ClientSecret, RedirectURI: redirect,
			Scopes: []string{"openid", "email", "profile", "groups"}, InsecureEnableGroups: true, GetUserInfo: true}
		return json.Marshal(cfg)
	}
	return nil, fmt.Errorf("unknown connector type %q", spec.Type)
}

// Add creates or replaces a connector.
func (s *ConnectorStore) Add(ctx context.Context, spec ConnectorSpec) (existed bool, err error) {
	if err := spec.Validate(); err != nil {
		return false, err
	}
	if s.Issuer == "" {
		return false, errors.New("the issuer URL (SHPYRD_AUTH_URL) is unknown; is auth-local enabled?")
	}
	raw, err := s.config(spec)
	if err != nil {
		return false, err
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": ConnectorGVR.Group + "/" + ConnectorGVR.Version,
		"kind":       "Connector",
		"metadata": map[string]interface{}{
			"name": spec.ID, "namespace": s.Namespace,
			"labels": map[string]interface{}{"app.kubernetes.io/managed-by": "shpyrd"},
		},
		"id":   spec.ID,
		"type": spec.Type,
		"name": spec.Name,
		// Dex declares config as []byte: base64 of the JSON.
		"config": base64.StdEncoding.EncodeToString(raw),
	}}
	if _, err := s.res().Create(ctx, obj, metav1.CreateOptions{}); err == nil {
		return false, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return false, wrap(err)
	}
	cur, err := s.res().Get(ctx, spec.ID, metav1.GetOptions{})
	if err != nil {
		return true, wrap(err)
	}
	obj.SetResourceVersion(cur.GetResourceVersion())
	_, err = s.res().Update(ctx, obj, metav1.UpdateOptions{})
	return true, wrap(err)
}

// List returns the connectors sorted by id, without their secrets.
func (s *ConnectorStore) List(ctx context.Context) ([]Connector, error) {
	list, err := s.res().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]Connector, 0, len(list.Items))
	for _, u := range list.Items {
		c := connectorOf(u)
		if c.ID == "" || c.Type == "local" {
			continue // Dex's own static connector
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func connectorOf(u unstructured.Unstructured) Connector {
	id, _, _ := unstructured.NestedString(u.Object, "id")
	typ, _, _ := unstructured.NestedString(u.Object, "type")
	name, _, _ := unstructured.NestedString(u.Object, "name")
	c := Connector{ID: id, Type: typ, Name: name}
	if enc, _, _ := unstructured.NestedString(u.Object, "config"); enc != "" {
		if raw, err := base64.StdEncoding.DecodeString(enc); err == nil {
			var cfg struct {
				Orgs          []githubOrg `json:"orgs"`
				HostedDomains []string    `json:"hostedDomains"`
				Tenant        string      `json:"tenant"`
				Issuer        string      `json:"issuer"`
			}
			_ = json.Unmarshal(raw, &cfg)
			switch {
			case len(cfg.Orgs) > 0:
				c.Detail = "organisation " + cfg.Orgs[0].Name
			case len(cfg.HostedDomains) > 0:
				c.Detail = "domain " + cfg.HostedDomains[0]
			case cfg.Tenant != "":
				c.Detail = "tenant " + cfg.Tenant
			case cfg.Issuer != "":
				c.Detail = cfg.Issuer
			}
		}
	}
	return c
}

// Remove deletes a connector.
func (s *ConnectorStore) Remove(ctx context.Context, id string) error {
	err := s.res().Delete(ctx, id, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("no connector %q; see `shpyrd auth connector list`", id)
	}
	return wrap(err)
}

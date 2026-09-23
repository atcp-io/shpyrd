package authoidc

import (
	"context"
	"testing"

	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := &Store{Kube: kubefake.NewSimpleClientset(), Namespace: "shpyrd-system"}
	existed, err := s.Set(ctx, Provider{ID: "okta", Label: "Okta", Issuer: "https://acme.okta.com/", ClientID: "0oa1", ClientSecret: "s3"})
	if err != nil || existed {
		t.Fatalf("set: existed=%v err=%v", existed, err)
	}
	existed, err = s.Set(ctx, Provider{ID: "okta", Label: "Okta SSO", Issuer: "https://acme.okta.com", ClientID: "0oa1", ClientSecret: "s4", Scopes: []string{}})
	if err != nil || !existed {
		t.Fatalf("update: existed=%v err=%v", existed, err)
	}
	_, _ = s.Set(ctx, Provider{ID: "entra", Label: "Microsoft", Issuer: "https://login.microsoftonline.com/t/v2.0", ClientID: "c", ClientSecret: "x", Scopes: []string{"-"}})
	list, err := s.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %v", list, err)
	}
	okta := list[1]
	if okta.ID != "okta" || okta.Label != "Okta SSO" || okta.Issuer != "https://acme.okta.com" || okta.ClientSecret != "s4" {
		t.Errorf("okta = %+v", okta)
	}
	if len(okta.Scopes) != 0 {
		t.Errorf("explicit empty scopes must stay empty, got %v", okta.Scopes)
	}
	o := okta.OIDC()
	if o.Kind != "oidc" || o.Issuer != "https://acme.okta.com" || o.Password {
		t.Errorf("OIDC() = %+v", o)
	}
	if err := s.Remove(ctx, "okta"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "okta"); err == nil {
		t.Error("removing twice must fail")
	}
	if list, _ = s.List(ctx); len(list) != 1 || list[0].ID != "entra" {
		t.Errorf("after remove: %+v", list)
	}
}

func TestDefaultScopesWhenUnset(t *testing.T) {
	ctx := context.Background()
	s := &Store{Kube: kubefake.NewSimpleClientset(), Namespace: "ns"}
	if _, err := s.Set(ctx, Provider{ID: "okta", Issuer: "https://acme.okta.com", ClientID: "c", ClientSecret: "x"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Get(ctx, "okta")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Scopes) != 1 || p.Scopes[0] != "groups" || p.Label != "okta" {
		t.Errorf("defaults: %+v", p)
	}
}

func TestValidate(t *testing.T) {
	bad := []Provider{
		{ID: "Okta", Issuer: "https://x", ClientID: "c", ClientSecret: "s"},
		{ID: "local", Issuer: "https://x", ClientID: "c", ClientSecret: "s"},
		{ID: "okta", Issuer: "http://x", ClientID: "c", ClientSecret: "s"},
		{ID: "okta", Issuer: "https://x/.well-known/openid-configuration", ClientID: "c", ClientSecret: "s"},
		{ID: "okta", Issuer: "https://x", ClientID: "", ClientSecret: "s"},
	}
	for _, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	if err := (Provider{ID: "okta-eu", Issuer: "https://acme.okta.com", ClientID: "c", ClientSecret: "s"}).Validate(); err != nil {
		t.Error(err)
	}
}

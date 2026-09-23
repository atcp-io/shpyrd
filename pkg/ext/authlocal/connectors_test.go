package authlocal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func TestConnectorStore(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "dex.coreos.com", Version: "v1", Kind: "Connector"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ConnectorList"), &unstructured.UnstructuredList{})
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{ConnectorGVR: "ConnectorList"})
	s := &ConnectorStore{Dynamic: dyn, Namespace: "shpyrd-system", Issuer: "https://auth.example.test/"}

	existed, err := s.Add(ctx, ConnectorSpec{Type: "github", ClientID: "gh1", ClientSecret: "ghs", Org: "acme"})
	if err != nil || existed {
		t.Fatalf("add: existed=%v err=%v", existed, err)
	}
	// Dex reads type, name, id and a base64 JSON config with its callback.
	u, err := dyn.Resource(ConnectorGVR).Namespace("shpyrd-system").Get(ctx, "github", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	typ, _, _ := unstructured.NestedString(u.Object, "type")
	name, _, _ := unstructured.NestedString(u.Object, "name")
	enc, _, _ := unstructured.NestedString(u.Object, "config")
	raw, _ := base64.StdEncoding.DecodeString(enc)
	var cfg githubConfig
	_ = json.Unmarshal(raw, &cfg)
	if typ != "github" || name != "GitHub" || cfg.ClientSecret != "ghs" || cfg.RedirectURI != "https://auth.example.test/callback" || len(cfg.Orgs) != 1 || cfg.Orgs[0].Name != "acme" || cfg.TeamNameField != "slug" || cfg.LoadAllGroups {
		t.Errorf("stored connector: type=%s name=%s cfg=%+v", typ, name, cfg)
	}

	existed, err = s.Add(ctx, ConnectorSpec{Type: "google", ClientID: "g1", ClientSecret: "gs", HostedDomain: "acme.com", Name: "Acme Google"})
	if err != nil || existed {
		t.Fatalf("add google: %v %v", existed, err)
	}
	if existed, err = s.Add(ctx, ConnectorSpec{Type: "github", ClientID: "gh2", ClientSecret: "ghs2"}); err != nil || !existed {
		t.Fatalf("replace: existed=%v err=%v", existed, err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list[0].ID != "github" || list[0].Detail != "" || list[1].ID != "google" || list[1].Name != "Acme Google" || list[1].Detail != "domain acme.com" {
		t.Errorf("list = %+v", list)
	}
	if err := s.Remove(ctx, "google"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "google"); err == nil {
		t.Error("removing twice must fail")
	}
	if err := (&ConnectorSpec{Type: "okta", ClientID: "a", ClientSecret: "b"}).Validate(); err == nil {
		t.Error("unknown type accepted")
	}
	if err := (&ConnectorSpec{Type: "github", ID: "local", ClientID: "a", ClientSecret: "b"}).Validate(); err == nil {
		t.Error("id local accepted")
	}
}

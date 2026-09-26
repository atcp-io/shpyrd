package controller

import (
	"context"
	"net"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// fakeResolver answers CNAME and A lookups from maps.
type fakeResolver struct {
	cname map[string]string
	a     map[string][]string
}

func (f fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	if c, ok := f.cname[host]; ok {
		return c + ".", nil
	}
	return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if c, ok := f.cname[host]; ok {
		host = c
	}
	ips, ok := f.a[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	var out []net.IPAddr
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out, nil
}

func domainsApp() *shpyrdv1.App {
	return &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "app-shop", Generation: 1},
		Spec: shpyrdv1.AppSpec{
			Image:     "registry.test/shop@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Processes: map[string]shpyrdv1.Process{"web": {Replicas: ptr.To[int32](1)}},
			Domains:   []string{"www.myprod.com", "MyProd.com.", "shop.example.test", "www.myprod.com"},
		},
	}
}

// The default host is always served first; custom domains are normalised
// and deduplicated; hosts under the cluster domain ride the wildcard.
func TestDomainsHostsAndTLS(t *testing.T) {
	c := Config{Domain: "example.test", ClusterIssuer: "letsencrypt", WildcardTLS: true}.Defaults()
	app := domainsApp()
	hosts := c.domains(app)
	want := []string{"shop.example.test", "www.myprod.com", "myprod.com"}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %v", hosts)
	}
	for i := range want {
		if hosts[i] != want[i] {
			t.Errorf("hosts[%d] = %q, want %q", i, hosts[i], want[i])
		}
	}
	tls := c.ingressTLS(app)
	if len(tls) != 3 || tls[0].SecretName != "" || tls[1].SecretName != certificateSecretName(app, "www.myprod.com") || tls[2].SecretName != certificateSecretName(app, "myprod.com") {
		t.Errorf("tls = %+v", tls)
	}
	// Without the wildcard the default host has its own certificate too.
	c.WildcardTLS = false
	if tls := c.ingressTLS(app); tls[0].SecretName == "" {
		t.Errorf("default host needs a certificate without the wildcard: %+v", tls[0])
	}
	if !c.underClusterDomain("x.example.test") || c.underClusterDomain("a.b.example.test") || c.underClusterDomain("example.test") {
		t.Error("underClusterDomain: direct children only")
	}
}

// Reconciling creates one Certificate per custom domain (and none for the
// default host when the wildcard serves it), and reports DNS per host.
func TestDomainsCertificatesAndStatus(t *testing.T) {
	app := domainsApp()
	r, c := newTestReconciler(t, app)
	r.Config.WildcardTLS = true
	r.Config.ClusterIssuer = "letsencrypt"
	r.Config.ExternalLBAddress = "147.15.59.84"
	r.Resolver = fakeResolver{
		cname: map[string]string{"www.myprod.com": "shop.example.test"},
		a:     map[string][]string{"shop.example.test": {"147.15.59.84"}, "myprod.com": {"1.2.3.4"}},
	}
	got := runReconcile(t, r, app)

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "CertificateList"})
	if err := c.List(context.Background(), list, client.InNamespace("app-shop")); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, it := range list.Items {
		names[it.GetName()] = true
	}
	if len(names) != 2 || !names[certificateSecretName(app, "www.myprod.com")] || !names[certificateSecretName(app, "myprod.com")] {
		t.Errorf("certificates = %v", names)
	}

	if len(got.Status.Domains) != 2 {
		t.Fatalf("domain statuses = %+v", got.Status.Domains)
	}
	byHost := map[string]shpyrdv1.DomainStatus{}
	for _, d := range got.Status.Domains {
		byHost[d.Host] = d
	}
	if d := byHost["www.myprod.com"]; d.DNS != DNSOK || d.Certificate != CertIssuing || d.Target != "shop.example.test" || d.Address != "147.15.59.84" {
		t.Errorf("www: %+v", d)
	}
	if d := byHost["myprod.com"]; d.DNS != DNSWrong || d.Message == "" {
		t.Errorf("apex pointing elsewhere: %+v", d)
	}

	// Removing a domain removes its certificate.
	got.Spec.Domains = []string{"www.myprod.com"}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r, got)
	if err := c.List(context.Background(), list, client.InNamespace("app-shop")); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].GetName() != certificateSecretName(app, "www.myprod.com") {
		t.Errorf("after removal: %d certificates", len(list.Items))
	}
}

// A host with no record at all is "missing", with the record to create.
func TestDomainsDNSMissing(t *testing.T) {
	r, _ := newTestReconciler(t)
	r.Resolver = fakeResolver{a: map[string][]string{}}
	if st := r.dnsState(context.Background(), "nothing.example.org", "shop.example.test", "147.15.59.84"); st != DNSMissing {
		t.Errorf("state = %s", st)
	}
	// An A record straight at the front door counts.
	r.Resolver = fakeResolver{a: map[string][]string{"apex.example.org": {"147.15.59.84"}}}
	if st := r.dnsState(context.Background(), "apex.example.org", "shop.example.test", "147.15.59.84"); st != DNSOK {
		t.Errorf("state = %s", st)
	}
}

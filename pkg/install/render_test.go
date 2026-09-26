package install

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/shpyrd-io/shpyrd/deploy"
)

func TestLocalProfileRenders(t *testing.T) {
	testProfileRenders(t, "local", map[string]string{VarDomain: "example.test", VarHTTPSPort: "8443"}, "https://auth.example.test:8443")
}

// The cloud profile renders with every variable defined and URLs without a
// port (a load balancer listens on 443). Since RFC-0059 it runs the same
// in-cluster registry as the local profile, with the platform CA generated
// in the cluster.
func TestOCIProfileRenders(t *testing.T) {
	eng := testProfileRenders(t, "oci", map[string]string{VarDomain: "oci.example.com", VarACMEEmail: "ops@example.com"}, "https://auth.oci.example.com")
	if eng.vars[VarClusterIssuer] != "letsencrypt" || eng.vars[VarURLPort] != "443" || eng.vars[VarRegistrySecret] != RegistrySecretName || eng.vars[VarCASource] != CASourceCluster {
		t.Errorf("oci vars: issuer=%s urlport=%s registrysecret=%s ca=%s", eng.vars[VarClusterIssuer], eng.vars[VarURLPort], eng.vars[VarRegistrySecret], eng.vars[VarCASource])
	}
	for _, present := range []string{"letsencrypt-issuers", "ca-issuers", "trust-manager", "registry-credentials", "registry", "registry-nodes", "ingress-nginx", "kpack", "shpyrd"} {
		if eng.components[present] == nil {
			t.Errorf("oci profile must install %s", present)
		}
	}
}

func TestAWSProfileRenders(t *testing.T) {
	eng := testProfileRenders(t, "aws", map[string]string{VarDomain: "aws.example.com", VarACMEEmail: "ops@example.com", VarDNSProvider: "aws", VarDNSZoneID: "Z123", VarDNSRegion: "us-east-1", VarEFSID: "fs-0123", VarAWSCluster: "shpyrd-dev", VarAWSRegion: "us-east-1", VarAWSVPCID: "vpc-1", VarAWSLBEIPs: "eipalloc-1,eipalloc-2"}, "https://auth.aws.example.com")
	if eng.vars[VarClusterIssuer] != "letsencrypt" || eng.vars[VarNetworkPolicy] != "none" || eng.vars[VarStorageClass] != "gp3" || eng.vars[VarWildcardTLS] != "true" {
		t.Errorf("aws vars: issuer=%s policy=%s class=%s wildcard=%s", eng.vars[VarClusterIssuer], eng.vars[VarNetworkPolicy], eng.vars[VarStorageClass], eng.vars[VarWildcardTLS])
	}
	for _, present := range []string{"aws-load-balancer-controller", "letsencrypt-issuers", "registry", "registry-nodes", "ingress-nginx", "ingress-nginx-internal", "external-dns", "dns", "snapshot-controller", "storage-gp3", "storage-efs", "kpack", "shpyrd"} {
		if eng.components[present] == nil {
			t.Errorf("aws profile must install %s", present)
		}
	}
	for _, absent := range []string{"network-policy", "dns01-oci", "storage-fss"} {
		if eng.components[absent] != nil {
			t.Errorf("aws profile must not install %s", absent)
		}
	}
	// The issuer comes from the profile overlay: Route 53, not the OCI webhook.
	objs, err := eng.renderComponent(eng.components["dns"])
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	for _, o := range objs {
		if o.GetKind() == "ClusterIssuer" {
			issuer = mustYAML(t, o)
		}
	}
	if !strings.Contains(issuer, "route53") || !strings.Contains(issuer, `"hostedZoneID":"Z123"`) || strings.Contains(issuer, "webhook") {
		t.Errorf("aws issuer:\n%s", issuer)
	}
	// The NLB has a hostname: no loadBalancerIP, the wildcard from the Service.
	vals, err := loadValues(deploy.FS, valuesFiles(deploy.FS, eng.components["ingress-nginx"], "aws"), eng.vars)
	if err != nil {
		t.Fatal(err)
	}
	svc := vals["controller"].(map[string]interface{})["service"].(map[string]interface{})
	ann := svc["annotations"].(map[string]interface{})
	if _, has := svc["loadBalancerIP"]; has || ann["service.beta.kubernetes.io/aws-load-balancer-type"] != "external" || ann["service.beta.kubernetes.io/aws-load-balancer-nlb-target-type"] != "ip" || ann["service.beta.kubernetes.io/aws-load-balancer-eip-allocations"] != "eipalloc-1,eipalloc-2" || ann["external-dns.kubernetes.io/hostname"] != "*.aws.example.com" {
		t.Errorf("aws ingress-nginx service values: %v", svc)
	}
	if _, oci := ann["oci.oraclecloud.com/load-balancer-type"]; oci {
		t.Errorf("OCI annotations leaked into the aws profile: %v", ann)
	}
}

// Every profile links the registry credential to the builder ServiceAccount
// and gives the kpack controller the trust bundle (RFC-0059).
func TestKpackRendersRegistryTrust(t *testing.T) {
	for _, profile := range []string{"local", "oci"} {
		eng, err := New(nil, Options{Profile: profile, Vars: map[string]string{VarDomain: "example.test"}, Reporter: &quiet{}})
		if err != nil {
			t.Fatal(err)
		}
		objs, err := eng.renderComponent(eng.components["kpack"])
		if err != nil {
			t.Fatal(err)
		}
		var sa, controller bool
		for _, o := range objs {
			y := mustYAML(t, o)
			if o.GetKind() == "ServiceAccount" && o.GetName() == "kpack-builder" {
				sa = strings.Contains(y, RegistrySecretName)
			}
			if o.GetKind() == "Deployment" && o.GetName() == "kpack-controller" {
				controller = strings.Contains(y, "SSL_CERT_FILE") && strings.Contains(y, CABundleName)
			}
		}
		if !sa || !controller {
			t.Errorf("%s: kpack-builder secret=%v controller trust=%v", profile, sa, controller)
		}
	}
}

// The registry serves TLS from the platform CA with the ClusterIP as a SAN
// and authenticates against the generated htpasswd; nodes get the CA from
// the registry-nodes DaemonSet.
func TestRegistryRendersTLS(t *testing.T) {
	eng, err := New(nil, Options{Profile: "local", Vars: map[string]string{VarDomain: "example.test"}, Reporter: &quiet{}})
	if err != nil {
		t.Fatal(err)
	}
	objs, err := eng.renderComponent(eng.components["registry"])
	if err != nil {
		t.Fatal(err)
	}
	var cert, config bool
	for _, o := range objs {
		y := mustYAML(t, o)
		switch {
		case o.GetKind() == "Certificate":
			cert = strings.Contains(y, "10.96.0.50") && strings.Contains(y, "shpyrd-ca")
		case o.GetKind() == "ConfigMap":
			config = strings.Contains(y, "tls:") && strings.Contains(y, "htpasswd") && !strings.Contains(y, "http: true")
		}
	}
	if !cert || !config {
		t.Errorf("registry: certificate=%v tls+auth config=%v", cert, config)
	}
	objs, err = eng.renderComponent(eng.components["registry-nodes"])
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.GetKind() == "DaemonSet" && !strings.Contains(mustYAML(t, o), "10.96.0.50:5000") {
			t.Error("registry-nodes must carry the registry host")
		}
	}
}

func testProfileRenders(t *testing.T, profile string, vars map[string]string, wantAuthURL string) *Engine {
	t.Helper()
	// Extension components render with the same profile.
	exts := []ExtensionComponent{{Extension: "auth-local", Component: "dex", Runlevel: "rc3"}}
	eng, err := New(nil, Options{Profile: profile, Vars: vars, Extensions: exts, Reporter: &quiet{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := eng.vars[VarExtensions]; got != "auth-local" {
		t.Errorf("SHPYRD_EXTENSIONS = %q", got)
	}
	if got := eng.vars[VarAuthURL]; got != wantAuthURL {
		t.Errorf("SHPYRD_AUTH_URL = %q, want %q", got, wantAuthURL)
	}
	comps, err := eng.profile.Components(deploy.FS)
	if err != nil {
		t.Fatalf("Components: %v", err)
	}
	if len(comps) == 0 {
		t.Fatal("profile has no components")
	}
	if eng.components["dex"] == nil {
		t.Error("extension component dex must join the profile")
	}
	if _, err := New(nil, Options{Profile: profile, Extensions: []ExtensionComponent{{Extension: "x", Component: "dex", Runlevel: "rc9"}}, Reporter: &quiet{}}); err == nil {
		t.Error("unknown runlevel must be refused")
	}
	for _, c := range comps {
		if c.Helm == nil && c.Kustomize == nil && len(c.Hooks) == 0 {
			t.Errorf("%s: neither helm, kustomize nor hooks", c.Name)
		}
		if c.Helm != nil {
			if c.Helm.Chart == "" || c.Helm.Version == "" {
				t.Errorf("%s: helm chart and version must be pinned", c.Name)
			}
			if _, err := loadValues(deploy.FS, valuesFiles(deploy.FS, c, profile), eng.vars); err != nil {
				t.Errorf("%s: values: %v", c.Name, err)
			}
		}
		if c.Kustomize != nil {
			objs, err := eng.renderComponent(c)
			if err != nil {
				t.Errorf("%s: render: %v", c.Name, err)
				continue
			}
			if len(objs) == 0 {
				t.Errorf("%s: rendered nothing", c.Name)
			}
			for _, o := range objs {
				if o.GetKind() == "" || o.GetName() == "" {
					t.Errorf("%s: object without kind/name: %v", c.Name, o.Object)
				}
				if strings.Contains(mustYAML(t, o), "${SHPYRD_") {
					t.Errorf("%s: unsubstituted variable in %s/%s", c.Name, o.GetKind(), o.GetName())
				}
			}
		}
		for _, w := range c.Wait {
			if w.String() == "unknown" {
				t.Errorf("%s: empty wait spec", c.Name)
			}
		}
	}
	return eng
}

func TestSubstitute(t *testing.T) {
	vars := map[string]string{"SHPYRD_DOMAIN": "example.test"}
	out, err := Substitute([]byte("host: app.${SHPYRD_DOMAIN}\nkeep: $1 and ${OTHER}\n"), vars)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "host: app.example.test\nkeep: $1 and ${OTHER}\n" {
		t.Errorf("unexpected output %q", out)
	}
	if _, err := Substitute([]byte("${SHPYRD_MISSING}"), vars); err == nil {
		t.Error("expected error for undefined variable")
	}
}

func TestSortObjects(t *testing.T) {
	mk := func(kind string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": kind, "metadata": map[string]interface{}{"name": "x"}}}
	}
	objs := []*unstructured.Unstructured{mk("ValidatingWebhookConfiguration"), mk("ClusterBuilder"), mk("Deployment"), mk("Namespace"), mk("CustomResourceDefinition"), mk("ServiceAccount")}
	sortObjects(objs)
	var got []string
	for _, o := range objs {
		got = append(got, o.GetKind())
	}
	want := "Namespace,ServiceAccount,CustomResourceDefinition,Deployment,ClusterBuilder,ValidatingWebhookConfiguration"
	if strings.Join(got, ",") != want {
		t.Errorf("got %s want %s", strings.Join(got, ","), want)
	}
}

func mustYAML(t *testing.T, o *unstructured.Unstructured) string {
	b, err := o.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type quiet struct{}

func (quiet) Runlevel(string, []string)  {}
func (quiet) Step(string, string)        {}
func (quiet) Done(string, time.Duration) {}
func (quiet) Failed(string, error)       {}

// The oci profile enforces NetworkPolicy with Calico in policy-only mode
// (RFC-0035): the upstream manifest with Oracle's edits for VCN-native pod
// networking.
func TestNetworkPolicyRendersForOKE(t *testing.T) {
	eng, err := New(nil, Options{Profile: "oci", Vars: map[string]string{VarDomain: "oci.example.com", VarACMEEmail: "ops@example.com"}, Reporter: &quiet{}})
	if err != nil {
		t.Fatal(err)
	}
	if eng.vars[VarNetworkPolicy] != "calico" || eng.components["network-policy"] == nil {
		t.Fatalf("oci profile must install network-policy (SHPYRD_NETWORK_POLICY=%s)", eng.vars[VarNetworkPolicy])
	}
	objs, err := eng.renderComponent(eng.components["network-policy"])
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.GetKind() != "DaemonSet" || o.GetName() != "calico-node" {
			continue
		}
		y := mustYAML(t, o)
		for _, want := range []string{"FELIX_INTERFACEPREFIX", "NO_DEFAULT_POOLS", "FELIX_CHAININSERTMODE", "FELIX_IPTABLESBACKEND", "CALICO_NETWORKING_BACKEND"} {
			if !strings.Contains(y, want) {
				t.Errorf("calico-node lacks %s", want)
			}
		}
		for _, absent := range []string{"initContainers", "FELIX_TYPHAK8SSERVICENAME", "cni-bin-dir", "cni-net-dir", "cni-log-dir", "kubernetes-services-endpoint"} {
			if strings.Contains(y, absent) {
				t.Errorf("calico-node still has %s", absent)
			}
		}
		return
	}
	t.Error("calico-node DaemonSet not rendered")
}

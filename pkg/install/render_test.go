package install

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"shpyrd/deploy"
)

func TestLocalProfileRenders(t *testing.T) {
	testProfileRenders(t, "local", map[string]string{VarDomain: "example.test", VarHTTPSPort: "8443"}, "https://auth.example.test:8443")
}

// The cloud profile renders with every variable defined and URLs without a
// port (a load balancer listens on 443).
func TestOCIProfileRenders(t *testing.T) {
	eng := testProfileRenders(t, "oci", map[string]string{VarDomain: "oci.example.com", VarACMEEmail: "ops@example.com", VarRegistryHost: "gru.ocir.io/ns"}, "https://auth.oci.example.com")
	if eng.vars[VarClusterIssuer] != "letsencrypt" || eng.vars[VarURLPort] != "443" || eng.vars[VarRegistrySecret] != RegistrySecretName {
		t.Errorf("oci vars: issuer=%s urlport=%s registrysecret=%s", eng.vars[VarClusterIssuer], eng.vars[VarURLPort], eng.vars[VarRegistrySecret])
	}
	for _, absent := range []string{"registry", "ca-issuers", "trust-manager"} {
		if eng.components[absent] != nil {
			t.Errorf("oci profile must not install %s", absent)
		}
	}
	for _, present := range []string{"letsencrypt-issuers", "registry-credentials", "ingress-nginx", "kpack", "shpyrd"} {
		if eng.components[present] == nil {
			t.Errorf("oci profile must install %s", present)
		}
	}
	// The kpack overlay links the registry Secret to the builder ServiceAccount.
	objs, err := eng.renderComponent(eng.components["kpack"])
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range objs {
		if o.GetKind() == "ServiceAccount" && o.GetName() == "kpack-builder" {
			found = strings.Contains(mustYAML(t, o), RegistrySecretName)
		}
	}
	if !found {
		t.Error("kpack-builder ServiceAccount must reference the registry Secret on the oci profile")
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

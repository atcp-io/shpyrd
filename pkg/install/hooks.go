package install

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"shpyrd/pkg/kube"
	"shpyrd/pkg/localca"
	"shpyrd/pkg/sizes"
)

// Hook runs before a component is applied. Hooks are referenced by name from
// component.yaml and implemented in Go for the few cases where manifests need
// data that only exists on the operator's machine.
type Hook func(ctx context.Context, e *Engine, c *Component) error

// LocalCASecretName is the Secret holding the local root CA for cert-manager.
const LocalCASecretName = "shpyrd-root-ca"

// CABundleName is the ConfigMap trust-manager distributes to every namespace
// with the cluster CA (key CABundleKey).
const (
	CABundleName = "shpyrd-ca-bundle"
	CABundleKey  = "ca-certificates.crt"
)

// AdminTokenSecretName holds the dashboard/API admin token (key "token").
const AdminTokenSecretName = "shpyrd-admin-token"

var hooks = map[string]Hook{
	"local-ca":             localCAHook,
	"admin-token":          adminTokenHook,
	"default-sizes":        defaultSizesHook,
	"oidc-client":          oidcClientHook,
	"registry-credentials": registryCredentialsHook,
}

// RegisterHook lets extensions add hooks their components reference.
func RegisterHook(name string, h Hook) { hooks[name] = h }

// Kube exposes the cluster client to hooks.
func (e *Engine) Kube() *kube.Client { return e.kube }

// Report prints a progress line for a component.
func (e *Engine) Report(component, message string) { e.rep.Step(component, message) }

// ApplyObject applies one object with server-side apply (for hooks).
func (e *Engine) ApplyObject(ctx context.Context, obj *unstructured.Unstructured, namespace string) error {
	return e.applier.applyOne(ctx, obj, namespace, false)
}

// OIDCClientSecretName holds the OpenID Connect client the dashboard uses at
// the login issuer (keys "client-id" and "client-secret"); created by the
// oidc-client hook and read by the server and by Dex.
const OIDCClientSecretName = "shpyrd-oidc-client"

// oidcClientHook generates the dashboard's OIDC client secret once.
func oidcClientHook(ctx context.Context, e *Engine, c *Component) error {
	ns := e.SystemNamespace()
	existing, err := e.kube.Kube.CoreV1().Secrets(ns).Get(ctx, OIDCClientSecretName, metav1.GetOptions{})
	if err == nil && len(existing.Data["client-secret"]) > 0 {
		e.rep.Step(c.Name, "keeping existing OIDC client secret")
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("oidc client: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	secret := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]interface{}{
			"name":      OIDCClientSecretName,
			"namespace": ns,
			"labels":    map[string]interface{}{"app.kubernetes.io/managed-by": fieldManager},
		},
		"stringData": map[string]interface{}{"client-id": "shpyrd", "client-secret": hex.EncodeToString(raw)},
	}}
	if err := e.applier.applyOne(ctx, secret, ns, false); err != nil {
		return fmt.Errorf("secret %s/%s: %w", ns, OIDCClientSecretName, err)
	}
	e.rep.Step(c.Name, "generated OIDC client secret")
	return nil
}

// defaultSizesHook seeds the instance size catalog on first install and
// leaves user edits alone afterwards.
func defaultSizesHook(ctx context.Context, e *Engine, c *Component) error {
	if _, err := e.kube.Kube.CoreV1().ConfigMaps(c.Namespace).Get(ctx, sizes.ConfigMapName, metav1.GetOptions{}); err == nil {
		e.rep.Step(c.Name, "keeping existing instance size catalog")
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("size catalog: %w", err)
	}
	data, err := sizes.Defaults().Marshal()
	if err != nil {
		return err
	}
	cm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      sizes.ConfigMapName,
			"namespace": c.Namespace,
			"labels":    map[string]interface{}{"app.kubernetes.io/managed-by": fieldManager},
		},
		"data": map[string]interface{}{sizes.ConfigMapKey: string(data)},
	}}
	if err := e.applier.applyOneAs(ctx, cm, c.Namespace, false, fieldManager+"-sizes-seed"); err != nil {
		return fmt.Errorf("size catalog: %w", err)
	}
	e.rep.Step(c.Name, "seeded instance size catalog (edit with `shpyrd sizes`)")
	return nil
}

// adminTokenHook creates a random admin token on first install and keeps the
// existing one afterwards. `shpyrd cluster token` prints it.
func adminTokenHook(ctx context.Context, e *Engine, c *Component) error {
	existing, err := e.kube.Kube.CoreV1().Secrets(c.Namespace).Get(ctx, AdminTokenSecretName, metav1.GetOptions{})
	if err == nil && len(existing.Data["token"]) > 0 {
		e.rep.Step(c.Name, "keeping existing admin token")
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("admin token: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := hex.EncodeToString(raw)
	secret := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]interface{}{
			"name":      AdminTokenSecretName,
			"namespace": c.Namespace,
			"labels":    map[string]interface{}{"app.kubernetes.io/managed-by": fieldManager},
		},
		"stringData": map[string]interface{}{"token": token},
	}}
	if err := e.applier.applyOne(ctx, secret, c.Namespace, false); err != nil {
		return fmt.Errorf("secret %s/%s: %w", c.Namespace, AdminTokenSecretName, err)
	}
	e.rep.Step(c.Name, "generated admin token (print it with `shpyrd cluster token`)")
	return nil
}

// localCAHook loads or generates the development root CA and stores it as a
// TLS Secret in the component namespace, where a cert-manager ClusterIssuer
// of type CA picks it up.
func localCAHook(ctx context.Context, e *Engine, c *Component) error {
	dir := e.opts.CADir
	if dir == "" {
		var err error
		dir, err = localca.DefaultDir()
		if err != nil {
			return err
		}
	}
	ca, created, err := localca.LoadOrCreate(dir)
	if err != nil {
		return fmt.Errorf("local CA: %w", err)
	}
	if created {
		e.rep.Step(c.Name, "generated development root CA at "+ca.CertPath())
	} else {
		e.rep.Step(c.Name, "using development root CA from "+ca.CertPath())
	}

	secret := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "kubernetes.io/tls",
		"metadata": map[string]interface{}{
			"name":      LocalCASecretName,
			"namespace": c.Namespace,
			"labels":    map[string]interface{}{"app.kubernetes.io/managed-by": fieldManager},
		},
		"data": map[string]interface{}{
			"tls.crt": base64.StdEncoding.EncodeToString(ca.CertPEM),
			"tls.key": base64.StdEncoding.EncodeToString(ca.KeyPEM),
			"ca.crt":  base64.StdEncoding.EncodeToString(ca.CertPEM),
		},
	}}
	if err := e.applier.applyOne(ctx, secret, c.Namespace, false); err != nil {
		return fmt.Errorf("secret %s/%s: %w", c.Namespace, LocalCASecretName, err)
	}
	return nil
}

// registryCredentialsHook writes the private registry's credentials as a
// dockerconfigjson Secret (RegistrySecretName) in the component namespace,
// where the kpack builder ServiceAccount links it and the App controller
// mirrors it into project namespaces for builds and image pulls. Without
// credentials on the command line an existing Secret is kept; a missing
// one is an error that says how to pass them.
func registryCredentialsHook(ctx context.Context, e *Engine, c *Component) error {
	secrets := e.kube.Kube.CoreV1().Secrets(c.Namespace)
	if e.opts.RegistryUser == "" || e.opts.RegistryPassword == "" {
		if _, err := secrets.Get(ctx, RegistrySecretName, metav1.GetOptions{}); err == nil {
			e.rep.Step(c.Name, "keeping existing registry credentials")
			return nil
		}
		return fmt.Errorf("the registry %s needs credentials: pass --registry-user and --registry-token-file (or --registry-password-env)", registryHostOf(e.vars[VarRegistryHost]))
	}
	host := registryHostOf(e.vars[VarRegistryHost])
	auth := base64.StdEncoding.EncodeToString([]byte(e.opts.RegistryUser + ":" + e.opts.RegistryPassword))
	cfg, _ := json.Marshal(map[string]interface{}{"auths": map[string]interface{}{
		host: map[string]string{"username": e.opts.RegistryUser, "password": e.opts.RegistryPassword, "auth": auth},
	}})
	secret := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "kubernetes.io/dockerconfigjson",
		"metadata": map[string]interface{}{
			"name":      RegistrySecretName,
			"namespace": c.Namespace,
			"labels":    map[string]interface{}{"app.kubernetes.io/managed-by": fieldManager},
		},
		"data": map[string]interface{}{".dockerconfigjson": base64.StdEncoding.EncodeToString(cfg)},
	}}
	if err := e.applier.applyOne(ctx, secret, c.Namespace, false); err != nil {
		return fmt.Errorf("secret %s/%s: %w", c.Namespace, RegistrySecretName, err)
	}
	e.rep.Step(c.Name, "stored credentials for "+host+" as "+e.opts.RegistryUser)
	return nil
}

// registryHostOf returns the host part of SHPYRD_REGISTRY_HOST, which may
// carry a path (gru.ocir.io/<tenancy-namespace>); docker auth is per host.
func registryHostOf(registry string) string {
	if i := strings.IndexByte(registry, '/'); i > 0 {
		return registry[:i]
	}
	return registry
}

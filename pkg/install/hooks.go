package install

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
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
	// A CA the cluster already has is never replaced: certificates issued
	// from it are trusted by nodes, builds and operator machines.
	if existing, err := e.kube.Kube.CoreV1().Secrets(c.Namespace).Get(ctx, LocalCASecretName, metav1.GetOptions{}); err == nil && len(existing.Data["tls.crt"]) > 0 {
		e.rep.Step(c.Name, "using the platform CA already in the cluster")
		return nil
	}
	var certPEM, keyPEM []byte
	if e.vars[VarCASource] == CASourceCluster {
		var err error
		certPEM, keyPEM, _, err = localca.Generate("shpyrd platform CA", e.vars[VarCluster])
		if err != nil {
			return fmt.Errorf("platform CA: %w", err)
		}
		e.rep.Step(c.Name, "generated the platform CA in the cluster (shpyrd cluster trust installs it on this machine)")
	} else {
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
		certPEM, keyPEM = ca.CertPEM, ca.KeyPEM
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
			"tls.crt": base64.StdEncoding.EncodeToString(certPEM),
			"tls.key": base64.StdEncoding.EncodeToString(keyPEM),
			"ca.crt":  base64.StdEncoding.EncodeToString(certPEM),
		},
	}}
	if err := e.applier.applyOne(ctx, secret, c.Namespace, false); err != nil {
		return fmt.Errorf("secret %s/%s: %w", c.Namespace, LocalCASecretName, err)
	}
	return nil
}

// registryCredentialsHook writes the registry's credentials as Secret
// shpyrd-registry (dockerconfigjson) in the system namespace: the controller
// mirrors it into every project for pushes and pulls. For the in-cluster
// registry (RFC-0059) it generates one platform credential and the htpasswd
// file the registry authenticates against; for an external registry it takes
// --registry-user and --registry-token-file. Existing credentials are kept.
func registryCredentialsHook(ctx context.Context, e *Engine, c *Component) error {
	secrets := e.kube.Kube.CoreV1().Secrets(c.Namespace)
	host := registryHostOf(e.vars[VarRegistryHost])
	inCluster := e.vars[VarRegistryIP] != ""

	user, password := e.opts.RegistryUser, e.opts.RegistryPassword
	// Credentials for other registries (a previous external registry) are
	// kept in the Secret so instances built from them can still be pulled
	// until their next release.
	auths := map[string]interface{}{}
	if existing, err := secrets.Get(ctx, RegistrySecretName, metav1.GetOptions{}); err == nil {
		auths = dockerConfigAuths(existing.Data[".dockerconfigjson"])
		if user == "" {
			if !inCluster {
				e.rep.Step(c.Name, "keeping existing registry credentials")
				return nil
			}
			// The htpasswd file must match the credential in use; rebuild it
			// from the stored credential when it is missing (upgrade from
			// an install without authentication).
			if _, err := secrets.Get(ctx, RegistryHtpasswdSecretName, metav1.GetOptions{}); err == nil {
				if u, _ := dockerConfigCredential(existing.Data[".dockerconfigjson"], host); u != "" {
					e.rep.Step(c.Name, "keeping existing registry credentials")
					return nil
				}
			}
			user, password = dockerConfigCredential(existing.Data[".dockerconfigjson"], host)
		}
	} else if user == "" && !inCluster {
		return fmt.Errorf("the registry %s needs credentials: pass --registry-user and --registry-token-file", host)
	}
	if user == "" {
		user = "shpyrd"
		raw := make([]byte, 24)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		password = hex.EncodeToString(raw)
	}

	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	auths[host] = map[string]string{"username": user, "password": password, "auth": auth}
	cfg, _ := json.Marshal(map[string]interface{}{"auths": auths})
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
	if inCluster {
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		htpasswd := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"type":       "Opaque",
			"metadata": map[string]interface{}{
				"name":      RegistryHtpasswdSecretName,
				"namespace": c.Namespace,
				"labels":    map[string]interface{}{"app.kubernetes.io/managed-by": fieldManager},
			},
			"data": map[string]interface{}{"htpasswd": base64.StdEncoding.EncodeToString([]byte(user + ":" + string(hash) + "\n"))},
		}}
		if err := e.applier.applyOne(ctx, htpasswd, c.Namespace, false); err != nil {
			return fmt.Errorf("secret %s/%s: %w", c.Namespace, RegistryHtpasswdSecretName, err)
		}
		e.rep.Step(c.Name, "in-cluster registry "+host+": credential "+user+" (Secret "+RegistrySecretName+")")
		return nil
	}
	e.rep.Step(c.Name, "stored credentials for "+host+" as "+user)
	return nil
}

// dockerConfigAuths returns the auths map of a dockerconfigjson document
// (empty when it cannot be read).
func dockerConfigAuths(raw []byte) map[string]interface{} {
	var cfg struct {
		Auths map[string]interface{} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Auths == nil {
		return map[string]interface{}{}
	}
	return cfg.Auths
}

// dockerConfigCredential extracts the user and password for host from a
// dockerconfigjson document.
func dockerConfigCredential(raw []byte, host string) (user, password string) {
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", ""
	}
	a, ok := cfg.Auths[host]
	if !ok {
		return "", ""
	}
	if a.Username != "" {
		return a.Username, a.Password
	}
	if dec, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
		if u, p, ok := strings.Cut(string(dec), ":"); ok {
			return u, p
		}
	}
	return "", ""
}

// registryHostOf returns the host part of SHPYRD_REGISTRY_HOST, which may
// carry a path (gru.ocir.io/<tenancy-namespace>); docker auth is per host.
func registryHostOf(registry string) string {
	if i := strings.IndexByte(registry, '/'); i > 0 {
		return registry[:i]
	}
	return registry
}

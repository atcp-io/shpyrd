package install

import (
	"context"
	"crypto"
	"crypto/md5"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
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
	"local-ca":                   localCAHook,
	"admin-token":                adminTokenHook,
	"default-sizes":              defaultSizesHook,
	"oidc-client":                oidcClientHook,
	"registry-credentials":       registryCredentialsHook,
	"object-storage-credentials": objectStorageCredentialsHook,
	"backup-target":              backupTargetHook,
	"dns-credentials":            dnsCredentialsHook,
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

// dnsCredentialsHook writes what the DNS automation authenticates with
// (RFC-0061): ExternalDNS's oci.yaml as Secret external-dns-config in the
// system namespace and, with an API key, the DNS-01 webhook's profile Secret
// in the cert-manager namespace. With workload identity there is no key:
// the oci.yaml only says so. Existing Secrets are kept when no key is given.
func dnsCredentialsHook(ctx context.Context, e *Engine, c *Component) error {
	provider := e.vars[VarDNSProvider]
	if provider == "" || provider == "none" {
		return nil
	}
	if provider == "aws" {
		// Route 53 through EKS Pod Identity: the role is attached to the
		// service accounts by the infrastructure; nothing to write.
		if e.vars[VarDNSZoneID] == "" || e.vars[VarDNSRegion] == "" {
			return errors.New("--dns aws needs --dns-zone-id and --dns-region (contrib/aws/terraform prints them)")
		}
		e.rep.Step(c.Name, "DNS automation through EKS Pod Identity (no key)")
		return nil
	}
	if provider != "oci" {
		return fmt.Errorf("DNS provider %q is not supported yet (oci and aws are)", provider)
	}
	compartment, tenancy, region := e.vars[VarDNSCompartment], e.vars[VarDNSTenancy], e.vars[VarDNSRegion]
	if compartment == "" || region == "" {
		return errors.New("--dns oci needs --dns-compartment and --dns-region (contrib/oci/terraform prints them)")
	}
	auth := e.vars[VarDNSAuth]
	secrets := e.kube.Kube.CoreV1().Secrets(c.Namespace)

	if auth == DNSAuthWorkload {
		cfg := fmt.Sprintf("auth:\n  region: %s\n  useWorkloadIdentity: true\ncompartment: %s\n", region, compartment)
		if err := e.applyOpaqueSecret(ctx, c.Namespace, DNSConfigSecretName, map[string]string{"oci.yaml": cfg}); err != nil {
			return err
		}
		e.rep.Step(c.Name, "DNS automation through OKE workload identity (no key)")
		return nil
	}

	user := e.vars[VarDNSUser]
	if e.opts.DNSKeyPEM == "" {
		if _, err := secrets.Get(ctx, DNSConfigSecretName, metav1.GetOptions{}); err == nil {
			e.rep.Step(c.Name, "keeping existing DNS credentials")
			return nil
		}
		return errors.New("--dns oci needs the API key of the DNS user: --dns-user and --dns-key-file (contrib/oci/terraform creates them with dns_auth = \"key\")")
	}
	if user == "" || tenancy == "" {
		return errors.New("--dns oci with a key needs --dns-user and --dns-tenancy")
	}
	fingerprint := e.opts.DNSKeyFingerprint
	if fingerprint == "" {
		var err error
		fingerprint, err = KeyFingerprint(e.opts.DNSKeyPEM)
		if err != nil {
			return fmt.Errorf("DNS key: %w", err)
		}
	}
	cfg := fmt.Sprintf("auth:\n  region: %s\n  tenancy: %s\n  user: %s\n  fingerprint: %s\n  key: |\n%s\ncompartment: %s\n",
		region, tenancy, user, fingerprint, indent(e.opts.DNSKeyPEM, "    "), compartment)
	if err := e.applyOpaqueSecret(ctx, c.Namespace, DNSConfigSecretName, map[string]string{"oci.yaml": cfg}); err != nil {
		return err
	}
	if err := e.applyOpaqueSecret(ctx, "cert-manager", DNSProfileSecretName, map[string]string{
		"tenancy": tenancy, "user": user, "region": region, "fingerprint": fingerprint,
		"privateKey": e.opts.DNSKeyPEM, "privateKeyPassphrase": "",
	}); err != nil {
		return err
	}
	e.rep.Step(c.Name, "DNS automation as user "+user+" (key "+fingerprint+")")
	return nil
}

// applyOpaqueSecret writes an Opaque Secret with server-side apply.
func (e *Engine) applyOpaqueSecret(ctx context.Context, namespace, name string, data map[string]string) error {
	enc := map[string]interface{}{}
	for k, v := range data {
		enc[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	secret := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"labels":    map[string]interface{}{"app.kubernetes.io/managed-by": fieldManager},
		},
		"data": enc,
	}}
	if err := e.applier.applyOne(ctx, secret, namespace, false); err != nil {
		return fmt.Errorf("secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// KeyFingerprint is OCI's fingerprint of an API signing key: the MD5 of the
// DER-encoded public key as colon-separated hex.
func KeyFingerprint(privateKeyPEM string) (string, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", errors.New("not a PEM private key")
	}
	var pub interface{}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		pub = &key.PublicKey
	} else if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		type publicKeyer interface{ Public() crypto.PublicKey }
		pk, ok := key.(publicKeyer)
		if !ok {
			return "", errors.New("unsupported private key type")
		}
		pub = pk.Public()
	} else {
		return "", errors.New("unsupported private key format (PKCS#1 or PKCS#8 expected)")
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := md5.Sum(der) //nolint:gosec // OCI defines the fingerprint as MD5
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, ":"), nil
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// Object storage (RFC-0046): the store's secrets, generated once and read
// only by the platform: the RPC secret Garage's nodes share, the admin
// token the controller uses, the metrics token.
const (
	ObjectStorageAdminSecretName = "object-storage-admin"
	ObjectStorageService         = "object-storage"
)

func objectStorageCredentialsHook(ctx context.Context, e *Engine, c *Component) error {
	secrets := e.kube.Kube.CoreV1().Secrets(c.Namespace)
	if _, err := secrets.Get(ctx, ObjectStorageAdminSecretName, metav1.GetOptions{}); err == nil {
		e.rep.Step(c.Name, "keeping existing object storage credentials")
		return nil
	}
	gen := func(n int) (string, error) {
		raw := make([]byte, n)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		return hex.EncodeToString(raw), nil
	}
	rpc, err := gen(32) // Garage wants exactly 32 bytes, hex encoded
	if err != nil {
		return err
	}
	admin, err := gen(24)
	if err != nil {
		return err
	}
	metrics, err := gen(24)
	if err != nil {
		return err
	}
	if err := e.applyOpaqueSecret(ctx, c.Namespace, ObjectStorageAdminSecretName, map[string]string{"rpcSecret": rpc, "adminToken": admin, "metricsToken": metrics}); err != nil {
		return err
	}
	e.rep.Step(c.Name, "generated the object storage credentials (Secret "+ObjectStorageAdminSecretName+")")
	return nil
}

// Platform backups (RFC-0037): the passphrase that encrypts every archive,
// generated once and printed by `shpyrd cluster backup key`, and the target
// (bucket, endpoint, region, credentials from --backup-credentials-file or
// none when the pod's cloud identity signs).
const (
	BackupKeySecretName    = "shpyrd-backup-key"
	BackupTargetSecretName = "platform-backup-target"
)

func backupTargetHook(ctx context.Context, e *Engine, c *Component) error {
	target := e.vars[VarBackupTarget]
	if target == "" {
		return errors.New("platform backups need a target: --backup-target s3://bucket/prefix (contrib/*/terraform prints it), or skip the component")
	}
	secrets := e.kube.Kube.CoreV1().Secrets(c.Namespace)
	if _, err := secrets.Get(ctx, BackupKeySecretName, metav1.GetOptions{}); err == nil {
		e.rep.Step(c.Name, "keeping the existing backup passphrase")
	} else {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		if err := e.applyOpaqueSecret(ctx, c.Namespace, BackupKeySecretName, map[string]string{"passphrase": hex.EncodeToString(raw)}); err != nil {
			return err
		}
		e.rep.Step(c.Name, "generated the backup passphrase (Secret "+BackupKeySecretName+"; print it with `shpyrd cluster backup key` and keep it elsewhere)")
	}
	data := map[string]string{
		"SHPYRD_BACKUP_TARGET":   target,
		"SHPYRD_BACKUP_ENDPOINT": e.vars[VarBackupEndpoint],
		"SHPYRD_BACKUP_REGION":   e.vars[VarBackupRegion],
	}
	if e.opts.BackupCredentials != nil {
		for k, v := range e.opts.BackupCredentials {
			data[k] = v
		}
	} else if existing, err := secrets.Get(ctx, BackupTargetSecretName, metav1.GetOptions{}); err == nil {
		// Keep credentials given on an earlier run.
		for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
			if v, ok := existing.Data[k]; ok {
				data[k] = string(v)
			}
		}
	}
	if err := e.applyOpaqueSecret(ctx, c.Namespace, BackupTargetSecretName, data); err != nil {
		return err
	}
	how := "the pod's cloud identity"
	if data["AWS_ACCESS_KEY_ID"] != "" {
		how = "an access key"
	}
	e.rep.Step(c.Name, "backups go to "+target+" ("+how+")")
	return nil
}

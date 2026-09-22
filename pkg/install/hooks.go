package install

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"shpyrd/pkg/localca"
	"shpyrd/pkg/sizes"
)

// Hook runs before a component is applied. Hooks are referenced by name from
// component.yaml and implemented in Go for the few cases where manifests need
// data that only exists on the operator's machine.
type Hook func(ctx context.Context, e *Engine, c *Component) error

// LocalCASecretName is the Secret holding the local root CA for cert-manager.
const LocalCASecretName = "shpyrd-root-ca"

// AdminTokenSecretName holds the dashboard/API admin token (key "token").
const AdminTokenSecretName = "shpyrd-admin-token"

var hooks = map[string]Hook{
	"local-ca":      localCAHook,
	"admin-token":   adminTokenHook,
	"default-sizes": defaultSizesHook,
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

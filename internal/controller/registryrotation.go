package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// AnnotationTLSChecksum on the registry's pod template carries a digest of
// the certificate it was started with.
const AnnotationTLSChecksum = "shpyrd.io/tls-checksum"

// RegistryRotation restarts the in-cluster registry when cert-manager
// renews its certificate (RFC-0059): Distribution reads the key pair once,
// at start, so without this the renewed certificate would sit unused in
// the Secret while the served one ran out.
type RegistryRotation struct {
	Client    client.Client
	Reader    client.Reader
	Namespace string // shpyrd-system
	Secret    string // registry-tls
	Deploy    string // registry
	Interval  time.Duration
}

// Start polls until the context ends (a manager Runnable).
func (r *RegistryRotation) Start(ctx context.Context) error {
	if r.Interval == 0 {
		r.Interval = 5 * time.Minute
	}
	logger := log.FromContext(ctx).WithName("registry-rotation")
	tick := time.NewTicker(r.Interval)
	defer tick.Stop()
	for {
		if restarted, err := r.Check(ctx); err != nil {
			logger.V(1).Info("check skipped", "err", err.Error())
		} else if restarted {
			logger.Info("registry restarted to pick up its renewed certificate")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Check compares the Secret's certificate with the one the registry runs
// with and restarts the Deployment when they differ. The first pass only
// records the current certificate.
func (r *RegistryRotation) Check(ctx context.Context) (bool, error) {
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	sec := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: r.Secret}, sec); err != nil {
		return false, err
	}
	crt := sec.Data[corev1.TLSCertKey]
	if len(crt) == 0 {
		return false, fmt.Errorf("secret %s has no %s", r.Secret, corev1.TLSCertKey)
	}
	sum := sha256.Sum256(crt)
	want := hex.EncodeToString(sum[:8])

	dep := &appsv1.Deployment{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: r.Deploy}, dep); err != nil {
		return false, err
	}
	have := dep.Spec.Template.Annotations[AnnotationTLSChecksum]
	if have == want {
		return false, nil
	}
	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[AnnotationTLSChecksum] = want
	if err := r.Client.Patch(ctx, dep, patch); err != nil {
		return false, err
	}
	// The first run stamps the running certificate; a stamp change on a
	// later run is the restart that matters, and both are one patch.
	return have != "", nil
}

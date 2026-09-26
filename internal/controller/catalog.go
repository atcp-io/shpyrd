package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/shpyrd-io/shpyrd/pkg/sizes"
)

// loadCatalog reads the instance size catalog, falling back to the built-in
// defaults when it is missing or invalid. Shared by every controller that
// sizes workloads.
func loadCatalog(ctx context.Context, c client.Client, systemNS string) sizes.Catalog {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: systemNS, Name: sizes.ConfigMapName}, cm); err != nil {
		return sizes.Defaults()
	}
	cat, err := sizes.Parse([]byte(cm.Data[sizes.ConfigMapKey]))
	if err != nil {
		log.FromContext(ctx).Info("size catalog invalid, using defaults", "err", err.Error())
		return sizes.Defaults()
	}
	return *cat
}

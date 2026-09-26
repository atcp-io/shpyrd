package kube

import (
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
)

// Scheme returns a runtime.Scheme with the core Kubernetes types and the
// shpyrd API registered.
func Scheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := shpyrdv1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// ControllerClient returns an uncached controller-runtime client that knows
// the shpyrd types, for CLI style read/modify/write access.
func (c *Client) ControllerClient() (client.Client, error) {
	s, err := Scheme()
	if err != nil {
		return nil, err
	}
	return client.New(c.Config, client.Options{Scheme: s, Mapper: c.Mapper})
}

package kube

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Client bundles the typed, dynamic and discovery clients for one cluster.
type Client struct {
	Config    *rest.Config
	Namespace string

	Kube      kubernetes.Interface
	Dynamic   dynamic.Interface
	Discovery discovery.CachedDiscoveryInterface
	Mapper    meta.RESTMapper
}

// NewClient builds a Client from a rest.Config. namespace is the default
// namespace used by Helm and by callers that do not specify one.
func NewClient(cfg *rest.Config, namespace string) (*Client, error) {
	if namespace == "" {
		namespace = "default"
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	cached := memory.NewMemCacheClient(dc)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cached)

	return &Client{
		Config:    cfg,
		Namespace: namespace,
		Kube:      kc,
		Dynamic:   dyn,
		Discovery: cached,
		Mapper:    mapper,
	}, nil
}

// Connect resolves opts into a ready Client.
func Connect(opts Options) (*Client, error) {
	cfg, ns, err := RESTConfig(opts)
	if err != nil {
		return nil, err
	}
	return NewClient(cfg, ns)
}

// InvalidateCache drops cached discovery data, e.g. after installing CRDs.
func (c *Client) InvalidateCache() {
	c.Discovery.Invalidate()
	if r, ok := c.Mapper.(meta.ResettableRESTMapper); ok {
		r.Reset()
	}
}

// RESTClientGetter adapts a Client to the interface Helm and kubectl
// libraries expect. Unlike genericclioptions.ConfigFlags it carries the
// complete rest.Config, so client certificates and exec plugins work.
type RESTClientGetter struct {
	client    *Client
	namespace string
}

// RESTClientGetterFor returns a getter scoped to namespace.
func (c *Client) RESTClientGetterFor(namespace string) *RESTClientGetter {
	if namespace == "" {
		namespace = c.Namespace
	}
	return &RESTClientGetter{client: c, namespace: namespace}
}

func (g *RESTClientGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(g.client.Config), nil
}

func (g *RESTClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return g.client.Discovery, nil
}

func (g *RESTClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	return g.client.Mapper, nil
}

// ToRawKubeConfigLoader returns a minimal loader whose only purpose is to
// report the namespace; Helm calls Namespace() on it.
func (g *RESTClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	overrides := &clientcmd.ConfigOverrides{
		Context: clientcmdapi.Context{Namespace: g.namespace},
	}
	return clientcmd.NewDefaultClientConfig(clientcmdapi.Config{}, overrides)
}

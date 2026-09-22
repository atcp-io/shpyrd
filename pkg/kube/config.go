// Package kube provides Kubernetes configuration discovery and a small client
// bundle shared by the CLI, the installer and the in-cluster server.
package kube

import (
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Options controls how a cluster connection is located.
type Options struct {
	// Kubeconfig is an explicit kubeconfig path. When empty the standard
	// loading rules apply ($KUBECONFIG, then ~/.kube/config).
	Kubeconfig string
	// Context selects a kubeconfig context. Empty means the current context.
	Context string
	// Namespace overrides the kubeconfig namespace. Empty means the context
	// namespace, falling back to "default".
	Namespace string
}

// ClientConfig returns the kubeconfig loader for opts, honouring $KUBECONFIG,
// the selected context and namespace override.
func (o Options) ClientConfig() clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.Kubeconfig != "" {
		rules.ExplicitPath = o.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if o.Context != "" {
		overrides.CurrentContext = o.Context
	}
	if o.Namespace != "" {
		overrides.Context = clientcmdapi.Context{Namespace: o.Namespace}
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
}

// InCluster reports whether the process appears to run inside a pod.
func InCluster() bool {
	return os.Getenv("KUBERNETES_SERVICE_HOST") != "" && os.Getenv("KUBERNETES_SERVICE_PORT") != ""
}

// RESTConfig resolves a rest.Config. Inside a pod the in-cluster service
// account is used unless an explicit kubeconfig or context was requested;
// otherwise the kubeconfig loading rules apply.
func RESTConfig(opts Options) (*rest.Config, string, error) {
	if InCluster() && opts.Kubeconfig == "" && opts.Context == "" {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, "", fmt.Errorf("in-cluster config: %w", err)
		}
		ns := opts.Namespace
		if ns == "" {
			ns = currentPodNamespace()
		}
		return cfg, ns, nil
	}

	cc := opts.ClientConfig()
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig: %w", err)
	}
	ns, _, err := cc.Namespace()
	if err != nil || ns == "" {
		ns = "default"
	}
	return cfg, ns, nil
}

func currentPodNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil && len(b) > 0 {
		return string(b)
	}
	return "default"
}

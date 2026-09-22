// Package kind creates and deletes local kind clusters shaped for shpyrd:
// host ports 80/443 mapped to the control-plane for ingress-nginx, the
// in-cluster registry NodePort mapped to the host, and a containerd mirror so
// nodes can pull images that kpack pushed to that registry.
package kind

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	kindcmd "sigs.k8s.io/kind/pkg/cmd"
	"sigs.k8s.io/kind/pkg/log"
)

// Config describes the cluster to create.
type Config struct {
	Name    string
	Workers int
	// NodeImage overrides the kind default node image.
	NodeImage string
	// HTTPPort and HTTPSPort are host ports mapped to the control-plane for
	// ingress. Set to 0 to skip.
	HTTPPort  int
	HTTPSPort int
	// RegistryHost is how workloads and nodes address the in-cluster
	// registry (host:port, plain HTTP). Nodes get a containerd hosts.toml
	// for it so image pulls use HTTP.
	RegistryHost string
	// RegistryNodePort is mapped to the same host port so the machine
	// running kind can push to localhost:RegistryNodePort.
	RegistryNodePort int
	// ServiceSubnet is pinned so the registry Service can use a fixed
	// ClusterIP that go-containerregistry treats as a plain HTTP endpoint.
	ServiceSubnet string
	// KubeconfigPath is where kind merges the cluster credentials. Empty
	// means the default kubeconfig.
	KubeconfigPath string
	// WaitForReady bounds the wait for the control-plane to be ready.
	WaitForReady time.Duration
	// AllowCgroupV1 relaxes the kubelet check that refuses cgroup v1 hosts
	// (recent Kubernetes releases fail by default). Create sets it
	// automatically when the Docker host reports cgroup v1.
	AllowCgroupV1 bool
}

// DefaultRegistryIP is the fixed ClusterIP of the in-cluster registry
// Service (inside the pinned service subnet). Because it is an RFC1918
// address, go-containerregistry based tools (kpack, the CNB lifecycle) use
// plain HTTP for it without any insecure-registry configuration.
const DefaultRegistryIP = "10.96.0.50"

// DefaultRegistryHost is how workloads and nodes address the registry.
const DefaultRegistryHost = DefaultRegistryIP + ":5000"

// Defaults returns the configuration used by `shpyrd cluster create`.
func Defaults(name string) Config {
	return Config{
		Name:             name,
		Workers:          1,
		HTTPPort:         80,
		HTTPSPort:        443,
		RegistryHost:     DefaultRegistryHost,
		RegistryNodePort: 30050,
		ServiceSubnet:    "10.96.0.0/16",
		WaitForReady:     3 * time.Minute,
	}
}

// ContextName is the kubeconfig context kind writes for the cluster.
func ContextName(name string) string { return "kind-" + name }

var configTemplate = template.Must(template.New("kind").Parse(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: {{ .Name }}
networking:
  serviceSubnet: {{ .ServiceSubnet }}
{{- if .AllowCgroupV1 }}
kubeadmConfigPatches:
  - |
    kind: KubeletConfiguration
    failCgroupV1: false
{{- end }}
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: InitConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        node-labels: "ingress-ready=true"
  extraPortMappings:
{{- if .HTTPPort }}
  - containerPort: 80
    hostPort: {{ .HTTPPort }}
    protocol: TCP
{{- end }}
{{- if .HTTPSPort }}
  - containerPort: 443
    hostPort: {{ .HTTPSPort }}
    protocol: TCP
{{- end }}
{{- if .RegistryNodePort }}
  - containerPort: {{ .RegistryNodePort }}
    hostPort: {{ .RegistryNodePort }}
    protocol: TCP
{{- end }}
{{- range $i := .WorkerSeq }}
- role: worker
{{- end }}
`))

// RenderConfig returns the kind cluster YAML for c.
func RenderConfig(c Config) ([]byte, error) {
	data := struct {
		Config
		WorkerSeq []int
	}{Config: c, WorkerSeq: make([]int, c.Workers)}
	var buf bytes.Buffer
	if err := configTemplate.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Provider wraps the kind cluster provider.
type Provider struct {
	p      *cluster.Provider
	logger log.Logger
}

// NewProvider uses Docker (kind's default) with a CLI logger.
func NewProvider() *Provider {
	logger := kindcmd.NewLogger()
	return &Provider{
		p:      cluster.NewProvider(cluster.ProviderWithLogger(logger), cluster.ProviderWithDocker()),
		logger: logger,
	}
}

// Exists reports whether a kind cluster with that name exists.
func (p *Provider) Exists(name string) (bool, error) {
	list, err := p.p.List()
	if err != nil {
		return false, err
	}
	for _, n := range list {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// List returns the names of existing kind clusters.
func (p *Provider) List() ([]string, error) { return p.p.List() }

// CgroupVersion reports the cgroup version of the Docker host ("1" or "2").
func CgroupVersion() (string, error) {
	out, err := exec.Command("docker", "info", "--format", "{{.CgroupVersion}}").Output()
	if err != nil {
		return "", fmt.Errorf("docker info: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Create creates the cluster and configures the registry mirror on each node.
func (p *Provider) Create(c Config) error {
	if !c.AllowCgroupV1 {
		if v, err := CgroupVersion(); err == nil && v == "1" {
			p.logger.Warn("Docker host uses cgroup v1, which Kubernetes deprecated; applying the kubelet override. Consider switching Docker Desktop to cgroup v2 (DeprecatedCgroupv1=false).")
			c.AllowCgroupV1 = true
		}
	}
	raw, err := RenderConfig(c)
	if err != nil {
		return err
	}
	opts := []cluster.CreateOption{
		cluster.CreateWithRawConfig(raw),
		cluster.CreateWithWaitForReady(c.WaitForReady),
		cluster.CreateWithDisplayUsage(false),
		cluster.CreateWithDisplaySalutation(false),
	}
	if c.NodeImage != "" {
		opts = append(opts, cluster.CreateWithNodeImage(c.NodeImage))
	}
	if c.KubeconfigPath != "" {
		opts = append(opts, cluster.CreateWithKubeconfigPath(c.KubeconfigPath))
	}
	if err := p.p.Create(c.Name, opts...); err != nil {
		return fmt.Errorf("kind create: %w", err)
	}
	if c.RegistryHost != "" {
		if err := p.ConfigureRegistryMirror(c.Name, c.RegistryHost); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the cluster and its kubeconfig entry.
func (p *Provider) Delete(name, kubeconfigPath string) error {
	if err := p.p.Delete(name, kubeconfigPath); err != nil {
		return fmt.Errorf("kind delete: %w", err)
	}
	return nil
}

// ConfigureRegistryMirror writes a containerd hosts.toml on every node so
// that pulls of <registryHost>/... use plain HTTP. Nodes reach the registry
// ClusterIP directly through kube-proxy. kind node images set
// config_path = /etc/containerd/certs.d, so no containerd restart is needed.
func (p *Provider) ConfigureRegistryMirror(name, registryHost string) error {
	nodeList, err := p.p.ListInternalNodes(name)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	hostsToml := fmt.Sprintf(`server = "http://%s"

[host."http://%s"]
  capabilities = ["pull", "resolve", "push"]
  skip_verify = true
`, registryHost, registryHost)
	dir := "/etc/containerd/certs.d/" + registryHost
	for _, n := range nodeList {
		if err := writeFile(n, dir+"/hosts.toml", hostsToml); err != nil {
			return fmt.Errorf("node %s: %w", n.String(), err)
		}
	}
	return nil
}

func writeFile(n nodes.Node, path, content string) error {
	dir := path[:strings.LastIndex(path, "/")]
	if err := n.Command("mkdir", "-p", dir).Run(); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	cmd := n.Command("cp", "/dev/stdin", path)
	cmd.SetStdin(strings.NewReader(content))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

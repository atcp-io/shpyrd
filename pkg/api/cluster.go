package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/ext/all"
	"shpyrd/pkg/install"
)

// ClusterSummary feeds the dashboard's cluster page.
type ClusterSummary struct {
	Install    *InstallInfo      `json:"install,omitempty"`
	Components []ComponentInfo   `json:"components"`
	Nodes      []NodeInfo        `json:"nodes"`
	Apps       int               `json:"apps"`
	Phases     map[string]int    `json:"phases"`
	Vars       map[string]string `json:"vars,omitempty"`
	// Extensions known to this build, with their state on the cluster.
	Extensions []ExtensionInfo `json:"extensions"`
	// Front doors (RFC-0036).
	ExternalLBAddress string `json:"externalLBAddress,omitempty"`
	InternalLBAddress string `json:"internalLBAddress,omitempty"`
}

// ExtensionInfo is one optional capability (RFC-0002).
type ExtensionInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Component   string `json:"component,omitempty"`
}

// InstallInfo mirrors the profile level install record.
type InstallInfo struct {
	Profile   string `json:"profile"`
	Version   string `json:"version"`
	Domain    string `json:"domain"`
	UpdatedAt string `json:"updatedAt"`
}

// ComponentInfo is one installed base stack component.
type ComponentInfo struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	AppliedAt string `json:"appliedAt"`
}

// NodeInfo is a cluster node.
type NodeInfo struct {
	Name           string `json:"name"`
	Ready          bool   `json:"ready"`
	Roles          string `json:"roles"`
	Arch           string `json:"arch"`
	OS             string `json:"os"`
	KubeletVersion string `json:"kubeletVersion"`
	CPU            string `json:"cpu"`
	Memory         string `json:"memory"`
	Pods           string `json:"pods"`
	// Machine shape and zone from the well-known topology labels; empty on
	// clusters without a cloud provider (kind).
	InstanceType string `json:"instanceType,omitempty"`
	Zone         string `json:"zone,omitempty"`
}

func (s *Server) clusterSummary(c *gin.Context) {
	ctx := c.Request.Context()
	out := ClusterSummary{Components: []ComponentInfo{}, Nodes: []NodeInfo{}, Phases: map[string]int{}}

	if info, err := install.ReadInstallInfo(ctx, s.kube, ""); err == nil {
		out.Install = &InstallInfo{Profile: info.Profile, Version: info.Version, Domain: info.Vars[install.VarDomain], UpdatedAt: info.UpdatedAt}
		out.Vars = info.Vars
	}
	out.Extensions = []ExtensionInfo{}
	for _, x := range all.All() {
		info := ExtensionInfo{Name: x.Name(), Description: x.Description(), Enabled: ext.Find(s.opts.Extensions, x.Name()) != nil}
		var names []string
		for _, c := range x.Components() {
			names = append(names, c.Name)
		}
		info.Component = strings.Join(names, ", ")
		out.Extensions = append(out.Extensions, info)
	}
	if recs, err := install.ReadRecords(ctx, s.kube, ""); err == nil {
		for _, r := range recs {
			out.Components = append(out.Components, ComponentInfo{Name: r.Name, Version: r.Version, AppliedAt: r.AppliedAt})
		}
		sort.Slice(out.Components, func(i, j int) bool { return out.Components[i].Name < out.Components[j].Name })
	}

	nodes, err := s.kube.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	for _, n := range nodes.Items {
		out.Nodes = append(out.Nodes, nodeInfo(n))
	}

	var apps shpyrdv1.AppList
	if err := s.apps.List(ctx, &apps); err == nil {
		out.Apps = len(apps.Items)
		for _, a := range apps.Items {
			phase := a.Status.Phase
			if phase == "" {
				phase = shpyrdv1.PhasePending
			}
			out.Phases[phase]++
		}
	}
	// Load balancer addresses for the cluster page's front-door summary.
	if svc, err := s.kube.Kube.CoreV1().Services("ingress-nginx").Get(ctx, "ingress-nginx-controller", metav1.GetOptions{}); err == nil {
		out.ExternalLBAddress = lbAddress(svc)
	}
	if svc, err := s.kube.Kube.CoreV1().Services("ingress-nginx-internal").Get(ctx, "ingress-nginx-internal-controller", metav1.GetOptions{}); err == nil {
		out.InternalLBAddress = lbAddress(svc)
	}
	c.JSON(http.StatusOK, out)
}

func nodeInfo(n corev1.Node) NodeInfo {
	info := NodeInfo{
		Name:           n.Name,
		Arch:           n.Status.NodeInfo.Architecture,
		OS:             n.Status.NodeInfo.OSImage,
		KubeletVersion: n.Status.NodeInfo.KubeletVersion,
		CPU:            n.Status.Capacity.Cpu().String(),
		Memory:         n.Status.Capacity.Memory().String(),
		Pods:           n.Status.Capacity.Pods().String(),
		InstanceType:   firstLabel(n.Labels, "node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type"),
		Zone:           firstLabel(n.Labels, "topology.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/zone"),
	}
	var roles []string
	for k := range n.Labels {
		if strings.HasPrefix(k, "node-role.kubernetes.io/") {
			role := strings.TrimPrefix(k, "node-role.kubernetes.io/")
			// Some providers (OKE) label plain workers "node"; keep the
			// dashboard vocabulary consistent with clusters that leave
			// workers unlabelled.
			if role == "node" {
				role = "worker"
			}
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	info.Roles = strings.Join(roles, ",")
	if info.Roles == "" {
		info.Roles = "worker"
	}
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			info.Ready = cond.Status == corev1.ConditionTrue
		}
	}
	return info
}

// firstLabel returns the value of the first key present in labels.
func firstLabel(labels map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// lbAddress is a load balancer Service's address: an IP where the cloud
// gives one, else its hostname (an NLB on AWS).
func lbAddress(svc *corev1.Service) string {
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			return in.IP
		}
	}
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if in.Hostname != "" {
			return in.Hostname
		}
	}
	return ""
}

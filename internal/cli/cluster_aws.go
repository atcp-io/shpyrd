package cli

import (
	"context"
	"fmt"
	"io"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shpyrd-io/shpyrd/pkg/install"
	"github.com/shpyrd-io/shpyrd/pkg/kube"
)

// discoverAWS fills the aws profile's cluster facts from the cluster
// itself when the operator did not pass them: the VPC CNI's DaemonSet
// carries the cluster name and the VPC, the nodes their region. What the
// cluster cannot know (Elastic IPs, the EFS file system, the Route 53 zone)
// comes from Terraform's vars file.
func discoverAWS(ctx context.Context, out io.Writer, k *kube.Client, vars map[string]string, prof *install.Profile) {
	need := func(key string) bool { return effectiveVar(vars, prof, key) == "" }
	if !need(install.VarAWSCluster) && !need(install.VarAWSVPCID) && !need(install.VarAWSRegion) && !need(install.VarDNSRegion) {
		return
	}
	var found []string
	if need(install.VarAWSCluster) || need(install.VarAWSVPCID) {
		if ds, err := k.Kube.AppsV1().DaemonSets("kube-system").Get(ctx, "aws-node", metav1.GetOptions{}); err == nil {
			for _, c := range ds.Spec.Template.Spec.Containers {
				for _, e := range c.Env {
					switch {
					case e.Name == "CLUSTER_NAME" && e.Value != "" && need(install.VarAWSCluster):
						vars[install.VarAWSCluster] = e.Value
						found = append(found, "cluster "+e.Value)
					case e.Name == "VPC_ID" && e.Value != "" && need(install.VarAWSVPCID):
						vars[install.VarAWSVPCID] = e.Value
						found = append(found, "VPC "+e.Value)
					}
				}
			}
		}
	}
	if need(install.VarAWSRegion) || need(install.VarDNSRegion) {
		if nodes, err := k.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1}); err == nil && len(nodes.Items) > 0 {
			if region := nodes.Items[0].Labels["topology.kubernetes.io/region"]; region != "" {
				if need(install.VarAWSRegion) {
					vars[install.VarAWSRegion] = region
					found = append(found, "region "+region)
				}
				if need(install.VarDNSRegion) && effectiveVar(vars, prof, install.VarDNSProvider) == "aws" {
					vars[install.VarDNSRegion] = region
				}
			}
		}
	}
	if len(found) > 0 {
		fmt.Fprintf(out, "AWS: %s (from the cluster)\n", joinComma(found))
	}
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

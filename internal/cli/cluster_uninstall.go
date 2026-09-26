package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/install"
	"github.com/shpyrd-io/shpyrd/pkg/kube"
)

// destroyCloud removes what the platform created in the cloud through
// Kubernetes, in the order that lets the cloud clean up after itself:
// projects (their namespaces take apps, databases, caches and volumes
// along), the load balancers, then every remaining claim (the registry, the
// server's data, monitoring). It waits until the cloud confirms each step
// and ends with the infrastructure command, which is Terraform's job.
func destroyCloud(ctx context.Context, cmd *cobra.Command, g *globalFlags, yes bool) error {
	k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
	if err != nil {
		return err
	}
	info, err := install.ReadInstallInfo(ctx, k, "")
	if err != nil {
		return fmt.Errorf("context %s does not run shpyrd (no install record): nothing to destroy here", g.kubeCtx)
	}
	c, err := k.ControllerClient()
	if err != nil {
		return err
	}
	return runDestroyCloud(ctx, cmd.OutOrStdout(), k, c, info, g.kubeCtx, func(q string) bool { return confirm(cmd, yes, q, false) })
}

func runDestroyCloud(ctx context.Context, out io.Writer, k *kube.Client, c client.Client, info *install.InstallInfo, contextName string, confirmFn func(string) bool) error {
	var apps shpyrdv1.AppList
	if err := c.List(ctx, &apps); err != nil {
		return err
	}
	var projects []string
	for _, a := range apps.Items {
		projects = append(projects, a.Name)
	}
	sort.Strings(projects)
	lbs, err := loadBalancerServices(ctx, k)
	if err != nil {
		return err
	}
	pvcs, err := k.Kube.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Cluster %s (profile %s, shpyrd %s) on context %s\n", info.Vars[install.VarCluster], info.Profile, info.Version, contextName)
	fmt.Fprintf(out, "  projects:       %d%s\n", len(projects), parenList(projects))
	fmt.Fprintf(out, "  load balancers: %d%s\n", len(lbs), parenList(lbs))
	fmt.Fprintf(out, "  disks:          %d claim(s), %s\n", len(pvcs.Items), claimsTotal(pvcs.Items))
	fmt.Fprintln(out, "This deletes every project with its data, the platform's load balancers and disks,")
	fmt.Fprintln(out, "so nothing outlives the cluster. The cluster itself, its network and DNS zone are")
	fmt.Fprintln(out, "removed by the infrastructure tooling afterwards (printed at the end).")
	if !confirmFn("Destroy the platform and all its data?") {
		return fmt.Errorf("aborted")
	}

	// 1. Projects: the namespace takes everything with it, including the
	// claims of volumes, databases and caches.
	if len(projects) > 0 {
		fmt.Fprintf(out, "Deleting %d project(s)...\n", len(projects))
		for _, slug := range projects {
			if err := k.Kube.CoreV1().Namespaces().Delete(ctx, appNamespace(slug), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete project %s: %w", slug, err)
			}
		}
		if err := waitFor(ctx, out, 10*time.Minute, "projects", func() (int, error) {
			left := 0
			for _, slug := range projects {
				if _, err := k.Kube.CoreV1().Namespaces().Get(ctx, appNamespace(slug), metav1.GetOptions{}); err == nil {
					left++
				}
			}
			return left, nil
		}); err != nil {
			return err
		}
	}

	// 1b. Ingresses: the platform's own hostnames. Removing them while
	// ExternalDNS still runs lets it withdraw the records it owns (its sync
	// policy deletes what it created); a minute is its reconcile interval.
	if ings, err := k.Kube.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{}); err == nil && len(ings.Items) > 0 {
		fmt.Fprintf(out, "Deleting %d ingress(es)...\n", len(ings.Items))
		for _, ing := range ings.Items {
			_ = k.Kube.NetworkingV1().Ingresses(ing.Namespace).Delete(ctx, ing.Name, metav1.DeleteOptions{})
		}
		if _, err := k.Kube.AppsV1().Deployments(install.DefaultSystemNamespace).Get(ctx, "external-dns", metav1.GetOptions{}); err == nil {
			fmt.Fprintln(out, "  giving ExternalDNS a minute to withdraw the DNS records it owns")
			if err := sleepCtx(ctx, 75*time.Second); err != nil {
				return err
			}
		}
	}

	// 2. Load balancers: the cloud controller removes the cloud object
	// before the Service finalizer lets go.
	if len(lbs) > 0 {
		fmt.Fprintf(out, "Deleting %d load balancer(s)...\n", len(lbs))
		for _, ref := range lbs {
			ns, name, _ := strings.Cut(ref, "/")
			if err := k.Kube.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete %s: %w", ref, err)
			}
		}
		if err := waitFor(ctx, out, 10*time.Minute, "load balancers", func() (int, error) {
			cur, err := loadBalancerServices(ctx, k)
			return len(cur), err
		}); err != nil {
			return err
		}
	}

	// The wildcard record hangs off the external front door's Service: the
	// same minute after the load balancers are gone.
	if len(lbs) > 0 {
		if _, err := k.Kube.AppsV1().Deployments(install.DefaultSystemNamespace).Get(ctx, "external-dns", metav1.GetOptions{}); err == nil {
			fmt.Fprintln(out, "  giving ExternalDNS a minute to withdraw the wildcard record")
			if err := sleepCtx(ctx, 75*time.Second); err != nil {
				return err
			}
		}
	}

	// 3. Disks: stop what holds them, then delete the claims; the CSI
	// driver deletes the cloud volume before the PersistentVolume goes.
	pvcs, err = k.Kube.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if len(pvcs.Items) > 0 {
		fmt.Fprintf(out, "Deleting %d disk(s)...\n", len(pvcs.Items))
		namespaces := map[string]bool{}
		for _, pvc := range pvcs.Items {
			namespaces[pvc.Namespace] = true
		}
		for ns := range namespaces {
			releaseClaims(ctx, k, ns)
		}
		for _, pvc := range pvcs.Items {
			if err := k.Kube.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete claim %s/%s: %w", pvc.Namespace, pvc.Name, err)
			}
		}
		if err := waitFor(ctx, out, 10*time.Minute, "disks", func() (int, error) {
			pvs, err := k.Kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return 0, err
			}
			n := 0
			for _, pv := range pvs.Items {
				if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
					n++
				}
			}
			return n, nil
		}); err != nil {
			return err
		}
		if pvs, err := k.Kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{}); err == nil && len(pvs.Items) > 0 {
			fmt.Fprintf(out, "Warning: %d volume(s) with a Retain policy stay in the cloud; delete them there if the data is not needed:\n", len(pvs.Items))
			for _, pv := range pvs.Items {
				fmt.Fprintf(out, "  %s (%s)\n", pv.Name, pv.Spec.Capacity.Storage().String())
			}
		}
	}

	// The record says the platform is gone, should anyone look before the
	// cluster is.
	_ = k.Kube.CoreV1().ConfigMaps(install.DefaultSystemNamespace).Delete(ctx, install.InstallRecordName, metav1.DeleteOptions{})

	fmt.Fprintln(out, "The platform's cloud resources are gone. Remove the infrastructure:")
	switch info.Profile {
	case "oci", "aws":
		fmt.Fprintf(out, "  cd contrib/%s/terraform && terraform destroy\n", info.Profile)
	default:
		fmt.Fprintln(out, "  destroy the cluster with the tooling that created it")
	}
	fmt.Fprintf(out, "Then forget the context: kubectl config delete-context %s\n", contextName)
	return nil
}

// loadBalancerServices lists Services of type LoadBalancer as ns/name.
func loadBalancerServices(ctx context.Context, k *kube.Client) ([]string, error) {
	svcs, err := k.Kube.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range svcs.Items {
		if s.Spec.Type == corev1.ServiceTypeLoadBalancer {
			out = append(out, s.Namespace+"/"+s.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// releaseClaims scales the workloads of a namespace to zero so their
// claims can be deleted (a claim in use waits for its pod).
func releaseClaims(ctx context.Context, k *kube.Client, ns string) {
	zero := int32(0)
	if deps, err := k.Kube.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, d := range deps.Items {
			if d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
				continue
			}
			d.Spec.Replicas = &zero
			_, _ = k.Kube.AppsV1().Deployments(ns).Update(ctx, &d, metav1.UpdateOptions{})
		}
	}
	if sets, err := k.Kube.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, s := range sets.Items {
			_ = k.Kube.AppsV1().StatefulSets(ns).Delete(ctx, s.Name, metav1.DeleteOptions{})
		}
	}
	// Anything else holding a claim (operators' pods) goes when its claim's
	// pod is deleted below.
	if pods, err := k.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, p := range pods.Items {
			for _, v := range p.Spec.Volumes {
				if v.PersistentVolumeClaim != nil {
					_ = k.Kube.CoreV1().Pods(ns).Delete(ctx, p.Name, metav1.DeleteOptions{})
					break
				}
			}
		}
	}
}

// waitFor polls count until it reaches zero, reporting progress.
func waitFor(ctx context.Context, out io.Writer, timeout time.Duration, what string, count func() (int, error)) error {
	deadline := time.Now().Add(timeout)
	last := -1
	for {
		n, err := count()
		if err != nil {
			return err
		}
		if n == 0 {
			fmt.Fprintf(out, "  %s: done\n", what)
			return nil
		}
		if n != last {
			fmt.Fprintf(out, "  %s: %d left\n", what, n)
			last = n
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: %d still present after %s; check the cloud console and run again", what, n, timeout)
		}
		if err := sleepCtx(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

func parenList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) > 6 {
		return " (" + strings.Join(items[:6], ", ") + ", ...)"
	}
	return " (" + strings.Join(items, ", ") + ")"
}

func claimsTotal(pvcs []corev1.PersistentVolumeClaim) string {
	var total int64
	for _, pvc := range pvcs {
		if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			total += q.Value()
		} else if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			total += q.Value()
		}
	}
	return fmt.Sprintf("%.0f GiB", float64(total)/(1<<30))
}

// isKindContext reports whether a kubeconfig context name is the one kind
// gives its clusters.
func isKindContext(ctx string) bool { return strings.HasPrefix(ctx, "kind-") }

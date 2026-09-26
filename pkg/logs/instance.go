// Package logs holds shared log-related utilities used by both the API
// server and the controller (RFC-0022a).
package logs

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
)

// InstanceNames maps pods to stable human names ("web.1", "web.2") by
// process type and creation order, the way Heroku names dynos.
func InstanceNames(pods []corev1.Pod) map[string]string {
	byProc := map[string][]corev1.Pod{}
	for _, p := range pods {
		byProc[p.Labels[shpyrdv1.LabelProcess]] = append(byProc[p.Labels[shpyrdv1.LabelProcess]], p)
	}
	out := map[string]string{}
	for proc, list := range byProc {
		sort.Slice(list, func(i, j int) bool {
			if !list[i].CreationTimestamp.Equal(&list[j].CreationTimestamp) {
				return list[i].CreationTimestamp.Before(&list[j].CreationTimestamp)
			}
			return list[i].Name < list[j].Name
		})
		for i, p := range list {
			name := proc
			if name == "" {
				name = "app"
			}
			out[p.Name] = fmt.Sprintf("%s.%d", name, i+1)
		}
	}
	return out
}

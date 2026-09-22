package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"helm.sh/helm/v3/pkg/action"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Namespace is the API representation of a Kubernetes namespace.
type Namespace struct {
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
}

// HelmRelease is a trimmed view of a Helm release; manifests and values are
// intentionally omitted.
type HelmRelease struct {
	Name         string    `json:"name"`
	Namespace    string    `json:"namespace"`
	Chart        string    `json:"chart"`
	ChartVersion string    `json:"chartVersion"`
	AppVersion   string    `json:"appVersion,omitempty"`
	Status       string    `json:"status"`
	Revision     int       `json:"revision"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (s *Server) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) listNamespaces(c *gin.Context) {
	list, err := s.kube.Kube.CoreV1().Namespaces().List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]Namespace, 0, len(list.Items))
	for _, ns := range list.Items {
		out = append(out, Namespace{
			Name:      ns.Name,
			Status:    string(ns.Status.Phase),
			Labels:    ns.Labels,
			CreatedAt: ns.CreationTimestamp.Time,
		})
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) listHelmReleases(c *gin.Context) {
	list := action.NewList(s.helm)
	list.All = true
	list.AllNamespaces = true
	list.SetStateMask()
	releases, err := list.Run()
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]HelmRelease, 0, len(releases))
	for _, r := range releases {
		hr := HelmRelease{
			Name:      r.Name,
			Namespace: r.Namespace,
			Revision:  r.Version,
		}
		if r.Chart != nil && r.Chart.Metadata != nil {
			hr.Chart = r.Chart.Metadata.Name
			hr.ChartVersion = r.Chart.Metadata.Version
			hr.AppVersion = r.Chart.Metadata.AppVersion
		}
		if r.Info != nil {
			hr.Status = r.Info.Status.String()
			hr.UpdatedAt = r.Info.LastDeployed.Time
		}
		out = append(out, hr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	c.JSON(http.StatusOK, out)
}

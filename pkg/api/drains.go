package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/internal/controller"
)

// Log drains (RFC-0023). Project drains live in the project namespace and
// need project.resource; cluster drains live in the system namespace and
// need cluster.admin. Header values are written to a Secret and never
// returned.

// DrainView is a drain as shown to people: header names, never values.
type DrainView struct {
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Format    string   `json:"format"`
	Processes []string `json:"processes,omitempty"`
	Headers   []string `json:"headers,omitempty"`
	// Cluster says the drain receives every project's lines.
	Cluster        bool       `json:"cluster"`
	Phase          string     `json:"phase"`
	Message        string     `json:"message,omitempty"`
	LastDeliveryAt *time.Time `json:"lastDeliveryAt,omitempty"`
	Sent           int64      `json:"sent"`
	Errors         int64      `json:"errors"`
	CreatedAt      time.Time  `json:"createdAt"`
}

// CreateDrainRequest adds a drain. Headers are written to a Secret.
type CreateDrainRequest struct {
	Name      string            `json:"name,omitempty"`
	URL       string            `json:"url" binding:"required"`
	Format    string            `json:"format,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Processes []string          `json:"processes,omitempty"`
}

var drainNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)

// DrainName derives a name from the URL host when none is given.
func DrainName(given, rawURL string) (string, error) {
	if given != "" {
		if !drainNameRe.MatchString(given) {
			return "", fmt.Errorf("invalid drain name %q: lowercase letters, digits and dashes", given)
		}
		return given, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("give a name: the url has no host to derive one from")
	}
	name := strings.ToLower(u.Hostname())
	name = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > 40 {
		name = strings.TrimRight(name[:40], "-")
	}
	if !drainNameRe.MatchString(name) {
		return "", fmt.Errorf("cannot derive a name from %q; pass one", rawURL)
	}
	return name, nil
}

func drainView(d shpyrdv1.LogDrain, headers []string, systemNS string) DrainView {
	v := DrainView{
		Name: d.Name, URL: d.Spec.URL, Format: d.EffectiveFormat(), Processes: d.Spec.Processes, Headers: headers,
		Cluster: d.Namespace == systemNS, Phase: firstNonEmpty(d.Status.Phase, shpyrdv1.DrainPending), Message: d.Status.Message,
		Sent: d.Status.Sent, Errors: d.Status.Errors, CreatedAt: d.CreationTimestamp.Time,
	}
	if d.Status.LastDeliveryAt != nil {
		t := d.Status.LastDeliveryAt.Time
		v.LastDeliveryAt = &t
	}
	return v
}

// listDrainsIn lists the drains of one namespace with their header names.
func (s *Server) listDrainsIn(ctx context.Context, ns string) ([]DrainView, error) {
	var list shpyrdv1.LogDrainList
	if err := s.apps.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	out := make([]DrainView, 0, len(list.Items))
	for _, d := range list.Items {
		out = append(out, drainView(d, s.drainHeaderNames(ctx, d), s.kube.Namespace))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) drainHeaderNames(ctx context.Context, d shpyrdv1.LogDrain) []string {
	if d.Spec.HeadersFrom == nil {
		return nil
	}
	sec := &corev1.Secret{}
	if err := s.apps.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: d.Spec.HeadersFrom.Name}, sec); err != nil {
		return nil
	}
	names := make([]string, 0, len(sec.Data))
	for k := range sec.Data {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// createDrainIn validates and writes a drain (and its header Secret).
func (s *Server) createDrainIn(c *gin.Context, ns, project string) {
	var req CreateDrainRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	name, err := DrainName(req.Name, req.URL)
	if err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	d := &shpyrdv1.LogDrain{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}},
		Spec:       shpyrdv1.LogDrainSpec{URL: strings.TrimSpace(req.URL), Format: req.Format, Processes: req.Processes},
	}
	if err := controller.ValidateDrainURL(d.Spec.URL, d.EffectiveFormat()); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	for k := range req.Headers {
		if strings.TrimSpace(k) == "" || strings.ContainsAny(k, " :\r\n") {
			abort(c, http.StatusBadRequest, fmt.Errorf("invalid header name %q", k))
			return
		}
	}
	ctx := c.Request.Context()
	if len(req.Headers) > 0 {
		secName := "drain-" + name + "-headers"
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secName, Namespace: ns, Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}},
			Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{},
		}
		for k, v := range req.Headers {
			sec.Data[k] = []byte(v)
		}
		if err := s.apps.Create(ctx, sec); err != nil {
			if apierrors.IsAlreadyExists(err) {
				if err := s.apps.Update(ctx, sec); err != nil {
					abort(c, http.StatusBadGateway, err)
					return
				}
			} else {
				abort(c, http.StatusBadGateway, err)
				return
			}
		}
		d.Spec.HeadersFrom = &corev1.LocalObjectReference{Name: secName}
	}
	if err := s.apps.Create(ctx, d); err != nil {
		if apierrors.IsAlreadyExists(err) {
			abort(c, http.StatusConflict, fmt.Errorf("drain %q already exists", name))
			return
		}
		abort(c, http.StatusBadGateway, err)
		return
	}
	target := name + " -> " + d.Spec.URL
	if project == "" {
		s.audit(c, "", "drain.add", target, "cluster drain ("+d.EffectiveFormat()+")")
	} else {
		s.audit(c, project, "drain.add", target, d.EffectiveFormat())
	}
	headers := make([]string, 0, len(req.Headers))
	for k := range req.Headers {
		headers = append(headers, k)
	}
	sort.Strings(headers)
	c.JSON(http.StatusCreated, drainView(*d, headers, s.kube.Namespace))
}

// deleteDrainIn removes a drain and its header Secret.
func (s *Server) deleteDrainIn(c *gin.Context, ns, project, name string) {
	ctx := c.Request.Context()
	d := &shpyrdv1.LogDrain{}
	if err := s.apps.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, d); err != nil {
		abortNotFound(c, err, "drain")
		return
	}
	if d.Spec.HeadersFrom != nil {
		_ = s.apps.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: d.Spec.HeadersFrom.Name}})
	}
	if err := s.apps.Delete(ctx, d); client.IgnoreNotFound(err) != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	s.audit(c, project, "drain.remove", name, "")
	c.Status(http.StatusNoContent)
}

// ---- project scope ------------------------------------------------------------

func (s *Server) listProjectDrains(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	out, err := s.listDrainsIn(c.Request.Context(), app.Namespace)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) createProjectDrain(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	s.createDrainIn(c, app.Namespace, app.Name)
}

func (s *Server) deleteProjectDrain(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	s.deleteDrainIn(c, app.Namespace, app.Name, c.Param("name"))
}

// ---- cluster scope ------------------------------------------------------------

func (s *Server) listClusterDrains(c *gin.Context) {
	out, err := s.listDrainsIn(c.Request.Context(), s.kube.Namespace)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) createClusterDrain(c *gin.Context) { s.createDrainIn(c, s.kube.Namespace, "") }

func (s *Server) deleteClusterDrain(c *gin.Context) {
	s.deleteDrainIn(c, s.kube.Namespace, "", c.Param("name"))
}

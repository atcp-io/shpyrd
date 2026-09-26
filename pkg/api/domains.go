package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/internal/controller"
	"shpyrd/pkg/install"
	"shpyrd/pkg/store"
)

// Custom domains (RFC-0034): POST adds a hostname the owner points at the
// project's own hostname (CNAME) or the front door (A); DELETE removes it.
// The controller issues the certificate once DNS points here and reports
// per-host state in status.domains.

var hostnameRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

type domainRequest struct {
	Host string `json:"host" binding:"required"`
}

// DomainsResult is the answer to a domain change: the records to create and
// the state of every custom domain.
type DomainsResult struct {
	Host    string                  `json:"host,omitempty"`
	Target  string                  `json:"target"`
	Address string                  `json:"address,omitempty"`
	Domains []shpyrdv1.DomainStatus `json:"domains"`
	App     AppSummary              `json:"app"`
}

// normalizeHost lower-cases and validates a hostname; wildcards are refused
// (they need DNS-01 through the owner's zone).
func normalizeHost(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.SplitN(h, "/", 2)[0]
	switch {
	case h == "":
		return "", fmt.Errorf("a hostname is required")
	case strings.HasPrefix(h, "*."):
		return "", fmt.Errorf("wildcard domains are not supported: add the hostnames you serve")
	case len(h) > 253 || !hostnameRe.MatchString(h):
		return "", fmt.Errorf("%q is not a valid hostname", h)
	}
	return h, nil
}

func (s *Server) addDomain(c *gin.Context) {
	var req domainRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	host, err := normalizeHost(req.Host)
	if err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	slug := c.Param("slug")
	domain := s.opts.Public.Domain
	if host == domain || host == "shpyrd."+domain || host == "auth."+domain || host == "grafana."+domain {
		abort(c, http.StatusBadRequest, fmt.Errorf("%s belongs to the platform", host))
		return
	}
	ws, _ := s.tenant(c)
	if ws != nil && ws.Address != "" && (host == ws.Address || host == hostOnly(s.dashboardHostOf(ws))) {
		abort(c, http.StatusBadRequest, fmt.Errorf("%s is the workspace's own address", host))
		return
	}
	if host == slug+"."+s.appsDomainOf(ws) {
		abort(c, http.StatusBadRequest, fmt.Errorf("%s is already the project's hostname", host))
		return
	}
	// A host under another workspace's address is theirs to give.
	if owner, err := s.tenancy.Resolve(c.Request.Context(), host); err == nil && ws != nil && owner.ID != ws.ID {
		abort(c, http.StatusConflict, fmt.Errorf("%s belongs to another workspace", host))
		return
	}
	if owner := s.hostOwner(c.Request.Context(), host); owner != "" && owner != slug {
		abort(c, http.StatusConflict, fmt.Errorf("%s is already used by project %s", host, owner))
		return
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		for _, d := range a.Spec.Domains {
			if strings.EqualFold(strings.TrimSuffix(d, "."), host) {
				return fmt.Errorf("%s is already a domain of this project", host)
			}
		}
		a.Spec.Domains = append(a.Spec.Domains, host)
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "domain.add", app.Name, host)
	c.JSON(http.StatusOK, s.domainsResult(c.Request.Context(), app, host))
}

func (s *Server) removeDomain(c *gin.Context) {
	host, err := normalizeHost(c.Param("host"))
	if err != nil {
		abort(c, http.StatusBadRequest, err)
		return
	}
	app, err := s.mutateApp(c, func(a *shpyrdv1.App) error {
		kept := a.Spec.Domains[:0:0]
		found := false
		for _, d := range a.Spec.Domains {
			if strings.EqualFold(strings.TrimSuffix(d, "."), host) {
				found = true
				continue
			}
			kept = append(kept, d)
		}
		if !found {
			return fmt.Errorf("%s is not a domain of this project", host)
		}
		a.Spec.Domains = kept
		return nil
	})
	if err != nil {
		return
	}
	s.audit(c, app.Name, "domain.remove", app.Name, host)
	c.JSON(http.StatusOK, s.domainsResult(c.Request.Context(), app, ""))
}

func (s *Server) listDomains(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, s.domainsResult(c.Request.Context(), app, ""))
}

// domainsResult builds the answer: the CNAME target is the project's own
// hostname, the A target the front door's address.
func (s *Server) domainsResult(ctx context.Context, app *shpyrdv1.App, host string) DomainsResult {
	target := app.Name + "." + s.opts.Public.Domain
	if slug := workspaceOf(app); slug != store.DefaultWorkspace {
		if ws, err := s.store.Workspace(ctx, slug); err == nil {
			target = app.Name + "." + s.appsDomainOf(ws)
		}
	}
	res := DomainsResult{
		Host:    host,
		Target:  target,
		Address: s.frontDoorAddress(ctx, app),
		Domains: app.Status.Domains,
		App:     summarize(app),
	}
	if res.Domains == nil {
		res.Domains = []shpyrdv1.DomainStatus{}
	}
	// A domain just added has no status yet: show what to create right away.
	if host != "" {
		found := false
		for _, d := range res.Domains {
			if d.Host == host {
				found = true
			}
		}
		if !found {
			res.Domains = append(res.Domains, shpyrdv1.DomainStatus{
				Host: host, DNS: controller.DNSUnknown, Certificate: controller.CertIssuing,
				Target: res.Target, Address: res.Address,
				Message: fmt.Sprintf("create a DNS record: CNAME %s -> %s (or %s -> %s at a zone apex)", host, res.Target, ApexRecordType(res.Address), res.Address),
			})
		}
	}
	return res
}

// ApexRecordType is the record a zone apex (which cannot carry a CNAME)
// points at the front door with: A where the load balancer has addresses
// (one or several, comma-separated), ALIAS where it has a hostname only.
func ApexRecordType(address string) string {
	for _, a := range strings.Split(address, ",") {
		if net.ParseIP(strings.TrimSpace(a)) == nil {
			return "ALIAS"
		}
	}
	return "A"
}

// frontDoorAddress is the load balancer a project's hosts resolve to: the
// static addresses the profile reserved for the public front door when it
// has them (SHPYRD_LB_IP; several on AWS, one per zone), else what the
// Service reports.
func (s *Server) frontDoorAddress(ctx context.Context, app *shpyrdv1.App) string {
	ns, name := "ingress-nginx", "ingress-nginx-controller"
	if app.Spec.Exposure == "internal" {
		ns, name = "ingress-nginx-internal", "ingress-nginx-internal-controller"
	} else if fixed := s.vars(install.VarLBIP); fixed != "" {
		return fixed
	}
	svc, err := s.kube.Kube.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			return in.IP
		}
		if in.Hostname != "" {
			return in.Hostname
		}
	}
	return ""
}

// hostOwner returns the project already serving host, or "". ingress-nginx
// refuses a host claimed twice; saying who has it is more useful.
func (s *Server) hostOwner(ctx context.Context, host string) string {
	var list networkingv1.IngressList
	if err := s.apps.List(ctx, &list); err != nil {
		return ""
	}
	for _, ing := range list.Items {
		for _, rule := range ing.Spec.Rules {
			if strings.EqualFold(rule.Host, host) {
				if p := ing.Labels[shpyrdv1.LabelApp]; p != "" {
					return p
				}
				return ing.Namespace + "/" + ing.Name
			}
		}
	}
	return ""
}

package controller

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// Custom domains (RFC-0034). A project is always served at
// <slug>.<cluster domain>; that hostname is what a custom domain's DNS record
// points at (CNAME), or the front door's address for a zone apex (A). Once
// DNS points here, cert-manager proves control with HTTP-01 and issues a
// certificate per custom domain; nothing else is asked of the owner, as on
// Heroku and Render. Each custom domain has its own Certificate so one whose
// DNS is not ready yet never blocks the others or the project's own host.

// CertificateGVK is cert-manager's Certificate.
var CertificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// Domain states.
const (
	DNSOK      = "ok"
	DNSMissing = "missing"
	DNSWrong   = "wrong"
	DNSUnknown = "unknown"

	CertReady    = "ready"
	CertIssuing  = "issuing"
	CertFailed   = "failed"
	CertWildcard = "wildcard"
)

// defaultHost is the hostname every project is served at.
func (c Config) defaultHost(app *shpyrdv1.App) string {
	return app.Name + "." + c.Domain
}

// customDomains are the hosts the project added, normalised and without the
// default host (which is always served anyway).
func (c Config) customDomains(app *shpyrdv1.App) []string {
	def := c.defaultHost(app)
	seen := map[string]bool{def: true}
	var out []string
	for _, d := range app.Spec.Domains {
		h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// domains are all hosts the Ingress serves: the default first, then the
// custom ones.
func (c Config) domains(app *shpyrdv1.App) []string {
	return append([]string{c.defaultHost(app)}, c.customDomains(app)...)
}

// underClusterDomain says the host is covered by the platform's wildcard
// certificate (a direct child of the cluster domain).
func (c Config) underClusterDomain(host string) bool {
	suffix := "." + c.Domain
	return strings.HasSuffix(host, suffix) && !strings.Contains(strings.TrimSuffix(host, suffix), ".")
}

// certificateSecretName is the TLS Secret of one host.
func certificateSecretName(app *shpyrdv1.App, host string) string {
	name := strings.NewReplacer(".", "-", "*", "wildcard").Replace(host)
	if len(name) > 40 {
		name = name[:40]
	}
	return app.Name + "-" + strings.Trim(name, "-") + "-tls"
}

// ingressTLS is the TLS section of the project's Ingress: one entry per host.
// Hosts under the cluster domain use the wildcard when the front door serves
// it as its default certificate; every other host has its own Secret.
func (c Config) ingressTLS(app *shpyrdv1.App) []networkingv1.IngressTLS {
	var out []networkingv1.IngressTLS
	for _, h := range c.domains(app) {
		if c.WildcardTLS && c.underClusterDomain(h) {
			out = append(out, networkingv1.IngressTLS{Hosts: []string{h}})
			continue
		}
		out = append(out, networkingv1.IngressTLS{Hosts: []string{h}, SecretName: certificateSecretName(app, h)})
	}
	return out
}

// reconcileCertificates keeps one cert-manager Certificate per host that
// needs its own, and removes the ones for hosts that left.
func (r *AppReconciler) reconcileCertificates(ctx context.Context, app *shpyrdv1.App) error {
	wanted := map[string]bool{}
	if hasWeb(app) {
		for _, h := range r.Config.domains(app) {
			if r.Config.WildcardTLS && r.Config.underClusterDomain(h) {
				continue
			}
			name := certificateSecretName(app, h)
			wanted[name] = true
			cert := &unstructured.Unstructured{}
			cert.SetGroupVersionKind(CertificateGVK)
			cert.SetName(name)
			cert.SetNamespace(app.Namespace)
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
				cert.SetLabels(mergeMaps(cert.GetLabels(), map[string]string{shpyrdv1.LabelApp: app.Name, shpyrdv1.LabelManagedBy: "shpyrd", "shpyrd.io/host": name}))
				_ = unstructured.SetNestedField(cert.Object, name, "spec", "secretName")
				_ = unstructured.SetNestedStringSlice(cert.Object, []string{h}, "spec", "dnsNames")
				_ = unstructured.SetNestedMap(cert.Object, map[string]interface{}{"kind": "ClusterIssuer", "name": r.Config.ClusterIssuer}, "spec", "issuerRef")
				return controllerutil.SetControllerReference(app, cert, r.Scheme)
			}); err != nil {
				return fmt.Errorf("certificate for %s: %w", h, err)
			}
		}
	}
	// Before RFC-0034 the Ingress annotation made cert-manager's ingress-shim
	// create <app>-tls for every host; that object outlives the annotation.
	legacy := &unstructured.Unstructured{}
	legacy.SetGroupVersionKind(CertificateGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: app.Name + "-tls"}, legacy); err == nil && !wanted[legacy.GetName()] && !metav1.IsControlledBy(legacy, app) {
		if err := r.Delete(ctx, legacy); err != nil && !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("could not remove legacy certificate", "err", err.Error())
		}
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "CertificateList"})
	if err := r.List(ctx, list, client.InNamespace(app.Namespace), client.MatchingLabels{shpyrdv1.LabelApp: app.Name, shpyrdv1.LabelManagedBy: "shpyrd"}); err != nil {
		return nil // cert-manager may be absent in tests
	}
	for i := range list.Items {
		cert := &list.Items[i]
		if wanted[cert.GetName()] || !metav1.IsControlledBy(cert, app) {
			continue
		}
		if err := r.Delete(ctx, cert); err != nil && !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("could not remove certificate", "name", cert.GetName(), "err", err.Error())
		}
	}
	return nil
}

// domainStatuses checks DNS and certificates of the custom domains. It
// returns true when something is still pending, so the reconcile is
// requeued.
func (r *AppReconciler) domainStatuses(ctx context.Context, app *shpyrdv1.App) ([]shpyrdv1.DomainStatus, bool) {
	hosts := r.Config.customDomains(app)
	if len(hosts) == 0 || !hasWeb(app) {
		return nil, false
	}
	address := r.frontDoorAddress(ctx, app)
	target := r.Config.defaultHost(app)
	pending := false
	var out []shpyrdv1.DomainStatus
	for _, h := range hosts {
		st := shpyrdv1.DomainStatus{Host: h, Target: target, Address: address}
		st.DNS = r.dnsState(ctx, h, target, address)
		if r.Config.WildcardTLS && r.Config.underClusterDomain(h) {
			st.Certificate = CertWildcard
		} else {
			st.Certificate, st.Message = r.certificateState(ctx, app, h)
		}
		switch st.DNS {
		case DNSMissing:
			st.Message = fmt.Sprintf("create a DNS record: CNAME %s -> %s (or A -> %s at a zone apex)", h, target, address)
		case DNSWrong:
			st.Message = fmt.Sprintf("%s points elsewhere: change it to CNAME -> %s (or A -> %s)", h, target, address)
		}
		if st.DNS != DNSOK || (st.Certificate != CertReady && st.Certificate != CertWildcard) {
			pending = true
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, pending
}

// frontDoorAddress is the address a custom domain must resolve to: the
// internal load balancer for internal projects, else the external one.
func (r *AppReconciler) frontDoorAddress(ctx context.Context, app *shpyrdv1.App) string {
	if app.Spec.Exposure == "internal" && r.Config.InternalLBAddress != "" {
		return r.Config.InternalLBAddress
	}
	if r.Config.ExternalLBAddress != "" {
		return r.Config.ExternalLBAddress
	}
	if r.LookupLB != nil {
		return r.LookupLB(ctx)
	}
	return ""
}

// dnsState resolves host and compares with the front door: a CNAME to the
// project's hostname or an A record to its address both count.
func (r *AppReconciler) dnsState(ctx context.Context, host, target, address string) string {
	resolver := r.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if cname, err := resolver.LookupCNAME(lctx, host); err == nil {
		cname = strings.TrimSuffix(strings.ToLower(cname), ".")
		if cname == strings.ToLower(target) {
			return DNSOK
		}
	}
	ips, err := resolver.LookupIPAddr(lctx, host)
	if err != nil {
		var dnsErr *net.DNSError
		if asDNSError(err, &dnsErr) && dnsErr.IsNotFound {
			return DNSMissing
		}
		return DNSUnknown
	}
	if address == "" {
		// Without a known address, resolving at all is the best we can say.
		return DNSOK
	}
	targetIPs := map[string]bool{}
	for _, a := range strings.Split(address, ",") {
		targetIPs[strings.TrimSpace(a)] = true
	}
	if tips, err := resolver.LookupIPAddr(lctx, target); err == nil {
		for _, ip := range tips {
			targetIPs[ip.IP.String()] = true
		}
	}
	for _, ip := range ips {
		if targetIPs[ip.IP.String()] {
			return DNSOK
		}
	}
	return DNSWrong
}

func asDNSError(err error, out **net.DNSError) bool {
	for e := err; e != nil; {
		if d, ok := e.(*net.DNSError); ok {
			*out = d
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// certificateState reads the host's Certificate.
func (r *AppReconciler) certificateState(ctx context.Context, app *shpyrdv1.App, host string) (string, string) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(CertificateGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: certificateSecretName(app, host)}, cert); err != nil {
		return CertIssuing, "certificate requested"
	}
	conds, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	var ready, issuing bool
	msg := ""
	for _, c := range conds {
		m, _ := c.(map[string]interface{})
		t, _ := m["type"].(string)
		s, _ := m["status"].(string)
		message, _ := m["message"].(string)
		reason, _ := m["reason"].(string)
		switch t {
		case "Ready":
			ready = s == "True"
			if !ready && message != "" {
				msg = message
			}
		case "Issuing":
			issuing = s == "True"
			if message != "" {
				msg = message
			}
			if reason == "Failed" {
				return CertFailed, shortMessage(message)
			}
		}
	}
	if ready {
		return CertReady, ""
	}
	_ = issuing
	return CertIssuing, shortMessage(firstNonEmpty(msg, "waiting for the certificate authority"))
}

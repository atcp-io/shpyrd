// Package controller reconciles shpyrd App resources into kpack builds,
// Deployments, Services and Ingresses.
package controller

import (
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// Config carries cluster-level settings the controller needs.
type Config struct {
	// Domain is the wildcard domain apps are published under.
	Domain string
	// HTTPSPort is the port users reach ingress on (443 unless kind maps another).
	HTTPSPort string
	// RegistryHost is where built images are pushed (host:port).
	RegistryHost string
	// ClusterIssuer signs app certificates.
	ClusterIssuer string
	// IngressClass for app Ingresses.
	IngressClass string
	// DefaultBuilder is the kpack ClusterBuilder used when the App does not
	// name one.
	DefaultBuilder string
	// BuildCacheSize is the kpack cache volume size (e.g. "2Gi"); empty disables.
	BuildCacheSize string
}

// Defaults fills unset fields.
func (c Config) Defaults() Config {
	if c.Domain == "" {
		c.Domain = "127.0.0.1.nip.io"
	}
	if c.HTTPSPort == "" {
		c.HTTPSPort = "443"
	}
	if c.RegistryHost == "" {
		c.RegistryHost = "10.96.0.50:5000"
	}
	if c.ClusterIssuer == "" {
		c.ClusterIssuer = "shpyrd-ca"
	}
	if c.IngressClass == "" {
		c.IngressClass = "nginx"
	}
	if c.DefaultBuilder == "" {
		c.DefaultBuilder = "shpyrd"
	}
	if c.BuildCacheSize == "" {
		c.BuildCacheSize = "2Gi"
	}
	return c
}

// kpack GVKs (handled as unstructured to avoid importing kpack's module).
var (
	KpackImageGVK = schema.GroupVersionKind{Group: "kpack.io", Version: "v1alpha2", Kind: "Image"}
	KpackBuildGVK = schema.GroupVersionKind{Group: "kpack.io", Version: "v1alpha2", Kind: "Build"}
)

// processes returns the effective process map (default: one web process),
// sorted by name for deterministic reconciliation.
func processes(app *shpyrdv1.App) []namedProcess {
	m := app.Spec.Processes
	if len(m) == 0 {
		m = map[string]shpyrdv1.Process{"web": {}}
	}
	out := make([]namedProcess, 0, len(m))
	for name, p := range m {
		out = append(out, namedProcess{Name: name, Process: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type namedProcess struct {
	Name string
	shpyrdv1.Process
}

// port returns the port the process listens on, or 0.
func (p namedProcess) port() int32 {
	if p.Port != nil {
		return *p.Port
	}
	if p.Name == "web" {
		return shpyrdv1.DefaultWebPort
	}
	return 0
}

func (p namedProcess) replicas() int32 {
	if p.Replicas != nil {
		return *p.Replicas
	}
	return 1
}

// workloadName is the Deployment/Service name of a process.
func workloadName(app *shpyrdv1.App, process string) string {
	return app.Name + "-" + process
}

func commonLabels(app *shpyrdv1.App) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": app.Name,
		shpyrdv1.LabelManagedBy:  "shpyrd",
		shpyrdv1.LabelApp:        app.Name,
	}
}

func processLabels(app *shpyrdv1.App, process string) map[string]string {
	l := commonLabels(app)
	l[shpyrdv1.LabelProcess] = process
	return l
}

func selectorLabels(app *shpyrdv1.App, process string) map[string]string {
	return map[string]string{
		shpyrdv1.LabelApp:     app.Name,
		shpyrdv1.LabelProcess: process,
	}
}

// domains returns the hosts served by the web process.
func (c Config) domains(app *shpyrdv1.App) []string {
	if len(app.Spec.Domains) > 0 {
		return app.Spec.Domains
	}
	return []string{app.Name + "." + c.Domain}
}

// url is the public URL of the web process.
func (c Config) url(app *shpyrdv1.App) string {
	u := "https://" + c.domains(app)[0]
	if c.HTTPSPort != "" && c.HTTPSPort != "443" {
		u += ":" + c.HTTPSPort
	}
	return u
}

// imageTag is the repository kpack pushes builds of this app to.
func (c Config) imageTag(app *shpyrdv1.App) string {
	return c.RegistryHost + "/apps/" + app.Name
}

// desiredKpackImage renders the kpack Image that builds the App's source.
func (c Config) desiredKpackImage(app *shpyrdv1.App) *unstructured.Unstructured {
	builder := c.DefaultBuilder
	if app.Spec.Build != nil && app.Spec.Build.Builder != "" {
		builder = app.Spec.Build.Builder
	}
	source := map[string]interface{}{}
	switch {
	case app.Spec.Source.Git != nil:
		git := map[string]interface{}{"url": app.Spec.Source.Git.URL}
		rev := app.Spec.Source.Git.Revision
		if rev == "" {
			rev = "main"
		}
		git["revision"] = rev
		source["git"] = git
	case app.Spec.Source.Blob != nil:
		source["blob"] = map[string]interface{}{"url": app.Spec.Source.Blob.URL}
	}
	if app.Spec.Source.SubPath != "" {
		source["subPath"] = app.Spec.Source.SubPath
	}

	spec := map[string]interface{}{
		"tag":                      c.imageTag(app),
		"serviceAccountName":       "default",
		"builder":                  map[string]interface{}{"name": builder, "kind": "ClusterBuilder"},
		"source":                   source,
		"failedBuildHistoryLimit":  int64(5),
		"successBuildHistoryLimit": int64(10),
		"imageTaggingStrategy":     "BuildNumber",
	}
	if c.BuildCacheSize != "" {
		spec["cache"] = map[string]interface{}{"volume": map[string]interface{}{"size": c.BuildCacheSize}}
	}
	if app.Spec.Build != nil && len(app.Spec.Build.Env) > 0 {
		var env []interface{}
		for _, e := range app.Spec.Build.Env {
			env = append(env, map[string]interface{}{"name": e.Name, "value": e.Value})
		}
		spec["build"] = map[string]interface{}{"env": env}
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(KpackImageGVK)
	u.SetName(app.Name)
	u.SetNamespace(app.Namespace)
	u.SetLabels(commonLabels(app))
	u.Object["spec"] = spec
	return u
}

// Default process size, the equivalent of a PaaS dyno/machine size. Users
// override it per process (spec.processes.<name>.resources). Limits give
// the dashboard a 100% mark for CPU and memory.
var (
	DefaultCPURequest    = resource.MustParse("100m")
	DefaultMemoryRequest = resource.MustParse("128Mi")
	DefaultCPULimit      = resource.MustParse("1")
	DefaultMemoryLimit   = resource.MustParse("512Mi")
)

// processResources fills the resources a process left unset.
func processResources(in corev1.ResourceRequirements) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	for k, v := range in.Requests {
		out.Requests[k] = v
	}
	for k, v := range in.Limits {
		out.Limits[k] = v
	}
	if _, ok := out.Limits[corev1.ResourceCPU]; !ok {
		out.Limits[corev1.ResourceCPU] = DefaultCPULimit
	}
	if _, ok := out.Limits[corev1.ResourceMemory]; !ok {
		out.Limits[corev1.ResourceMemory] = DefaultMemoryLimit
	}
	if _, ok := out.Requests[corev1.ResourceCPU]; !ok {
		out.Requests[corev1.ResourceCPU] = minQuantity(DefaultCPURequest, out.Limits[corev1.ResourceCPU])
	}
	if _, ok := out.Requests[corev1.ResourceMemory]; !ok {
		out.Requests[corev1.ResourceMemory] = minQuantity(DefaultMemoryRequest, out.Limits[corev1.ResourceMemory])
	}
	return out
}

func minQuantity(a, b resource.Quantity) resource.Quantity {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// mutateDeployment sets the fields shpyrd owns on a process Deployment.
func (c Config) mutateDeployment(app *shpyrdv1.App, p namedProcess, image, configHash string, d *appsv1.Deployment) {
	labels := processLabels(app, p.Name)
	d.Labels = mergeMaps(d.Labels, labels)
	if d.Spec.Selector == nil {
		// The selector is immutable; only set it on creation.
		d.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(app, p.Name)}
	}
	d.Spec.Replicas = ptr.To(p.replicas())
	d.Spec.RevisionHistoryLimit = ptr.To[int32](3)

	container := corev1.Container{
		Name:      "app",
		Image:     image,
		Command:   p.Command,
		Args:      p.Args,
		Resources: processResources(p.Resources),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
		},
		EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: app.EnvSecretName()},
				Optional:             ptr.To(true),
			},
		}},
	}
	// Buildpack images expose every process type as /cnb/process/<type>;
	// "web" is the image entrypoint so it also works for plain images.
	if len(p.Command) == 0 && p.Name != "web" {
		container.Command = []string{"/cnb/process/" + p.Name}
	}
	if port := p.port(); port > 0 {
		container.Env = append(container.Env, corev1.EnvVar{Name: "PORT", Value: fmt.Sprint(port)})
		container.Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP}}
		container.ReadinessProbe = &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
			PeriodSeconds:       5,
			InitialDelaySeconds: 2,
		}
	}
	container.Env = append(container.Env, app.Spec.Env...)

	d.Spec.Template.Labels = mergeMaps(d.Spec.Template.Labels, labels)
	d.Spec.Template.Annotations = mergeMaps(d.Spec.Template.Annotations, map[string]string{
		shpyrdv1.AnnotationConfigHash: configHash,
	})
	d.Spec.Template.Spec.EnableServiceLinks = ptr.To(false)
	d.Spec.Template.Spec.Containers = []corev1.Container{container}
}

// mutateService sets the fields shpyrd owns on a process Service.
func (c Config) mutateService(app *shpyrdv1.App, p namedProcess, s *corev1.Service) {
	s.Labels = mergeMaps(s.Labels, processLabels(app, p.Name))
	s.Spec.Selector = selectorLabels(app, p.Name)
	s.Spec.Ports = []corev1.ServicePort{{
		Name:       "http",
		Port:       80,
		TargetPort: intstr.FromString("http"),
		Protocol:   corev1.ProtocolTCP,
	}}
}

// mutateIngress sets the fields shpyrd owns on the web Ingress.
func (c Config) mutateIngress(app *shpyrdv1.App, ing *networkingv1.Ingress) {
	ing.Labels = mergeMaps(ing.Labels, processLabels(app, "web"))
	ing.Annotations = mergeMaps(ing.Annotations, map[string]string{
		"cert-manager.io/cluster-issuer":              c.ClusterIssuer,
		"nginx.ingress.kubernetes.io/ssl-redirect":    "true",
		"nginx.ingress.kubernetes.io/proxy-body-size": "50m",
	})
	hosts := c.domains(app)
	ing.Spec.IngressClassName = ptr.To(c.IngressClass)
	ing.Spec.TLS = []networkingv1.IngressTLS{{Hosts: hosts, SecretName: app.Name + "-tls"}}
	pathType := networkingv1.PathTypePrefix
	rules := make([]networkingv1.IngressRule, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, networkingv1.IngressRule{
			Host: h,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path:     "/",
					PathType: &pathType,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: workloadName(app, "web"),
						Port: networkingv1.ServiceBackendPort{Name: "http"},
					}},
				}},
			}},
		})
	}
	ing.Spec.Rules = rules
}

func mergeMaps(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// shortImage trims a digest reference for display.
func shortImage(ref string) string {
	if i := strings.Index(ref, "@sha256:"); i >= 0 && len(ref) >= i+8+12 {
		return ref[:i] + "@" + ref[i+1:i+8+12]
	}
	return ref
}

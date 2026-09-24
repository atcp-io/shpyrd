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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/sizes"
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
	// SystemNamespace holds cluster-wide configuration such as the size
	// catalog.
	SystemNamespace string
	// BuildKitImage runs Dockerfile builds (rootless BuildKit).
	BuildKitImage string
	// PodCIDR, when known, lets the project network policy block egress to
	// pods of other projects while allowing the internet.
	PodCIDR string
	// RegistrySecret names the dockerconfigjson Secret in SystemNamespace
	// with the registry's credentials; "" when the registry needs none
	// (the in-cluster registry). It is mirrored into every project
	// namespace: builds push with it, instances pull with it.
	RegistrySecret string
	// RegistryInsecure says the registry speaks plain HTTP (an external
	// registry without TLS; the in-cluster registry serves TLS from the
	// platform CA since RFC-0059).
	RegistryInsecure bool
	// CABundle names the trust bundle ConfigMap trust-manager puts in every
	// namespace (public roots plus the platform CA); builds mount it so
	// they trust the in-cluster registry. "" mounts nothing.
	CABundle string
	// RegistryDeletes says images of pruned releases may be deleted from
	// the registry (the in-cluster one; provider registries keep their own
	// retention).
	RegistryDeletes bool
	// WildcardTLS says the front door serves the platform's wildcard
	// certificate by default (RFC-0061): project Ingresses get no
	// certificate of their own.
	WildcardTLS bool
	// Front doors (RFC-0036).
	IngressClassExternal string // default "nginx"
	IngressClassInternal string // default "nginx-internal"
	// InternalLBAddress is the address of the internal load balancer;
	// used as the ExternalDNS target for internal Ingresses.
	InternalLBAddress string
	// ExternalLBAddress is the public front door's address, what a custom
	// domain's A record points at (RFC-0034); "" when unknown at start.
	ExternalLBAddress string
}

// BuildServiceAccount is the ServiceAccount builds run as in a project
// namespace when the registry needs credentials.
const BuildServiceAccount = "shpyrd-builder"

// buildServiceAccountName is what kpack Images run as: the credentialed
// account when the registry needs one, else the namespace default.
func (c Config) buildServiceAccountName() string {
	if c.RegistrySecret != "" {
		return BuildServiceAccount
	}
	return "default"
}

// imagePullSecrets for project pods: the mirrored registry Secret, if any.
func (c Config) imagePullSecrets() []corev1.LocalObjectReference {
	if c.RegistrySecret == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: c.RegistrySecret}}
}

// DefaultBuildKitImage is the rootless BuildKit image used for Dockerfile builds.
// Fully qualified: CRI-O (OKE, OpenShift) refuses Docker Hub short names.
const DefaultBuildKitImage = "docker.io/moby/buildkit:v0.32.2-rootless"

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
	if c.IngressClassExternal == "" {
		c.IngressClassExternal = c.IngressClass
	}
	if c.IngressClassExternal == "" {
		c.IngressClassExternal = "nginx"
	}
	if c.IngressClassInternal == "" {
		c.IngressClassInternal = c.IngressClassExternal
	}
	c.IngressClass = c.IngressClassExternal
	if c.DefaultBuilder == "" {
		c.DefaultBuilder = "shpyrd"
	}
	if c.BuildCacheSize == "" {
		c.BuildCacheSize = "2Gi"
	}
	if c.SystemNamespace == "" {
		c.SystemNamespace = "shpyrd-system"
	}
	if c.BuildKitImage == "" {
		c.BuildKitImage = DefaultBuildKitImage
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
		"serviceAccountName":       c.buildServiceAccountName(),
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

// processResources resolves the instance size of a process against the
// cluster catalog (see pkg/sizes): explicit resources override, then the
// named size, then the catalog default. It returns the size name in effect.
func processResources(p namedProcess, catalog sizes.Catalog) (corev1.ResourceRequirements, string, error) {
	res, name, err := catalog.Resolve(p.Size, p.Resources)
	if err != nil {
		return corev1.ResourceRequirements{}, "", fmt.Errorf("process %s: %w", p.Name, err)
	}
	if name == "" {
		name = "custom"
	}
	return res, name, nil
}

// mutateDeployment sets the fields shpyrd owns on a process Deployment.
func (c Config) mutateDeployment(app *shpyrdv1.App, p namedProcess, image, configHash string, res corev1.ResourceRequirements, mounts []resolvedMount, d *appsv1.Deployment) {
	labels := processLabels(app, p.Name)
	d.Labels = mergeMaps(d.Labels, labels)
	if d.Spec.Selector == nil {
		// The selector is immutable; only set it on creation.
		d.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(app, p.Name)}
	}
	d.Spec.Replicas = ptr.To(p.replicas())
	d.Spec.RevisionHistoryLimit = ptr.To[int32](3)
	d.Spec.Strategy = rolloutStrategy(p, mounts)

	container := corev1.Container{
		Name:            "app",
		Image:           image,
		Command:         p.Command,
		Args:            p.Args,
		Resources:       res,
		SecurityContext: hardenedSecurityContext(),
		// Global vars, then the project's config vars, then the vars of
		// attached resources: with envFrom the last source wins, so project
		// vars override globals and bound vars win over both (RFC-0003,
		// RFC-0016).
		EnvFrom: EnvSources(app),
	}
	// Buildpack images expose every process type as /cnb/process/<type>;
	// "web" is the image entrypoint so it also works for plain images.
	if len(p.Command) == 0 && p.Name != "web" {
		container.Command = []string{"/cnb/process/" + p.Name}
	}
	port := p.port()
	if port > 0 {
		container.Env = append(container.Env, corev1.EnvVar{Name: "PORT", Value: fmt.Sprint(port)})
		container.Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP}}
	}
	applyProbes(&container, p, port)
	container.Env = append(container.Env, app.Spec.Env...)

	d.Spec.Template.Labels = mergeMaps(d.Spec.Template.Labels, labels)
	d.Spec.Template.Annotations = mergeMaps(d.Spec.Template.Annotations, map[string]string{
		shpyrdv1.AnnotationConfigHash: configHash,
	})
	if at := app.Annotations[shpyrdv1.AnnotationRestartedAt]; at != "" {
		// A redeploy: same release, new pods.
		d.Spec.Template.Annotations[shpyrdv1.AnnotationRestartedAt] = at
	}
	d.Spec.Template.Spec.EnableServiceLinks = ptr.To(false)
	d.Spec.Template.Spec.ImagePullSecrets = c.imagePullSecrets()
	d.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	hc := p.HealthCheck
	if hc == nil || !hc.Disabled {
		shutdown := int64(parseDurationSecs(hc.GetStr("ShutdownDelay"), 5))
		timeout := int64(parseDurationSecs(hc.GetStr("Timeout"), 5))
		tgp := shutdown + timeout + 5
		d.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(tgp)
	}
	d.Spec.Template.Spec.Containers = []corev1.Container{container}
	applyMounts(d, mounts)
}

// rolloutStrategy returns the Deployment strategy for a process. Processes
// with a RWO volume use Recreate; everything else uses RollingUpdate with
// maxSurge=1 and maxUnavailable=0 so traffic is always served (RFC-0019).
func rolloutStrategy(p namedProcess, mounts []resolvedMount) appsv1.DeploymentStrategy {
	for _, m := range mounts {
		if !m.Shared {
			return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		}
	}
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(1)),
			MaxUnavailable: ptr.To(intstr.FromInt32(0)),
		},
	}
}

// applyProbes configures the readiness, liveness and startup probes and the
// preStop lifecycle hook according to RFC-0019. Defaults by process type:
//   - web (port 8080): HTTP GET / on the port
//   - explicit port (non-web): TCP on that port
//   - no port (workers): no probe
func applyProbes(c *corev1.Container, p namedProcess, port int32) {
	hc := p.HealthCheck
	if hc != nil && hc.Disabled {
		return
	}
	interval := parseDurationSecs(hc.GetStr("Interval"), 10)
	timeout := parseDurationSecs(hc.GetStr("Timeout"), 5)
	grace := parseDurationSecs(hc.GetStr("GracePeriod"), 30)
	shutdown := parseDurationSecs(hc.GetStr("ShutdownDelay"), 5)

	var handler corev1.ProbeHandler
	switch {
	case hc != nil && len(hc.Command) > 0:
		handler = corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: hc.Command}}
	case hc != nil && hc.TCP:
		if port > 0 {
			handler = corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}}
		}
	case hc != nil && hc.Path != "":
		handler = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: hc.Path, Port: intstr.FromInt32(port)}}
	case p.Name == "web" && port > 0:
		handler = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(port)}}
	case port > 0:
		handler = corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}}
	default:
		return // workers without a port: no probe
	}

	readiness := &corev1.Probe{ProbeHandler: handler, PeriodSeconds: interval, TimeoutSeconds: timeout, FailureThreshold: 3, SuccessThreshold: 1}
	liveness := &corev1.Probe{ProbeHandler: handler, PeriodSeconds: interval, TimeoutSeconds: timeout, FailureThreshold: 6, SuccessThreshold: 1}
	startup := &corev1.Probe{ProbeHandler: handler, PeriodSeconds: 5, TimeoutSeconds: timeout, FailureThreshold: int32(grace / 5), SuccessThreshold: 1}
	if startup.FailureThreshold < 6 {
		startup.FailureThreshold = 6
	}
	c.ReadinessProbe = readiness
	c.LivenessProbe = liveness
	c.StartupProbe = startup

	gracePeriod := int64(shutdown) + int64(timeout) + 5
	c.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	c.Lifecycle = &corev1.Lifecycle{
		PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{
			Command: []string{"sh", "-c", fmt.Sprintf("sleep %d", shutdown)},
		}},
	}
	_ = gracePeriod // applied on the pod template below (desired.go)
}

// parseDurationSecs parses a "Ns" or "Nm" string as seconds, or returns the
// default when the input is empty or invalid.
func parseDurationSecs(s string, def int32) int32 {
	if s == "" {
		return def
	}
	var n int32
	var unit string
	if _, err := fmt.Sscanf(s, "%d%s", &n, &unit); err != nil || n <= 0 {
		return def
	}
	if unit == "m" {
		return n * 60
	}
	return n
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

// mutateIngress sets the fields shpyrd owns on the web Ingress. With the
// platform's wildcard certificate as the front door's default (RFC-0061)
// the Ingress declares its hosts under TLS without a certificate of its own:
// ingress-nginx serves the default and no issuance happens per project.
func (c Config) mutateIngress(app *shpyrdv1.App, ing *networkingv1.Ingress) {
	ing.Labels = mergeMaps(ing.Labels, processLabels(app, "web"))
	ing.Annotations = mergeMaps(ing.Annotations, map[string]string{
		"nginx.ingress.kubernetes.io/ssl-redirect":    "true",
		"nginx.ingress.kubernetes.io/proxy-body-size": "50m",
	})
	hosts := c.domains(app)
	// Pick the ingress class and ExternalDNS target based on exposure.
	class := c.IngressClass // already set to IngressClassExternal by Defaults
	if app.Spec.Exposure == "internal" {
		if c.IngressClassInternal != "" {
			class = c.IngressClassInternal
		}
		// Point the host's A record at the private LB, not the public one.
		if c.InternalLBAddress != "" {
			ing.Annotations["external-dns.kubernetes.io/target"] = c.InternalLBAddress
		}
	} else {
		delete(ing.Annotations, "external-dns.kubernetes.io/target")
	}
	ing.Spec.IngressClassName = ptr.To(class)
	// Certificates are explicit objects (reconcileCertificates), one per host
	// that needs one, so the Ingress carries no cert-manager annotation.
	delete(ing.Annotations, "cert-manager.io/cluster-issuer")
	ing.Spec.TLS = c.ingressTLS(app)
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

// hardenedSecurityContext is the restricted Pod Security Standard for app
// containers (RFC-0008): non-root, no privilege escalation, no capabilities,
// the runtime's default seccomp profile. Buildpack images already run as a
// non-root user; Dockerfile images need a USER.
func hardenedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		RunAsNonRoot:             ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// EnvSources lists the Secrets a process reads its environment from, in
// precedence order (later wins): globals, the project's config vars, bound
// vars. Every source is optional: a project may have no config vars, no
// attachments or no globals yet. One-off commands use the same list.
func EnvSources(app *shpyrdv1.App) []corev1.EnvFromSource {
	optional := func(name string) corev1.EnvFromSource {
		return corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Optional:             ptr.To(true),
		}}
	}
	var out []corev1.EnvFromSource
	if !globalsDisabled(app) {
		out = append(out, optional(shpyrdv1.GlobalEnvSecretName))
	}
	return append(out, optional(app.EnvSecretName()), optional(app.BindingsSecretName()))
}

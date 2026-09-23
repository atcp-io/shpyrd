package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/project"
)

// Log drains (RFC-0023). Every LogDrain, whatever its namespace, ends up as
// a filter + sink pair in Vector's second config file, rendered into
// ConfigMap vector-drains in the logs-agent namespace. Header values never
// enter the config: they reach Vector as environment variables from the
// referenced Secrets, and Vector expands ${VAR} at load time.

const (
	// LogsNamespace is where the logs-agent extension runs Vector.
	LogsNamespace = "logs-system"
	// DrainsConfigMapName is the ConfigMap Vector reads its drain config from.
	DrainsConfigMapName = "vector-drains"
	// VectorDaemonSetName is the agent's DaemonSet.
	VectorDaemonSetName = "vector"
	// VectorMetrics is the agent's Prometheus exporter (one pod answers
	// behind the Service; every pod runs the same sinks, so counts are per
	// node and approximate).
	VectorMetrics = "http://vector.logs-system.svc:9598/metrics"
	// drainStatusPeriod is how often delivery status is refreshed.
	drainStatusPeriod = 30 * time.Second
)

// LogDrainReconciler renders drains into Vector's config and reports their
// delivery status.
type LogDrainReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// SystemNamespace holds cluster drains (every project's lines).
	SystemNamespace string
	// HTTP fetches Vector's metrics; nil disables status polling (tests).
	HTTP *http.Client
}

// SetupWithManager registers the controller: every LogDrain, the Secrets
// they reference, and the agent's ConfigMap (repaired when edited by hand).
func (r *LogDrainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.SystemNamespace == "" {
		r.SystemNamespace = "shpyrd-system"
	}
	if r.HTTP == nil {
		r.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("logdrain").
		For(&shpyrdv1.LogDrain{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToDrains)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.configMapToDrains)).
		Complete(r)
}

// secretToDrains requeues the drains that reference a Secret.
func (r *LogDrainReconciler) secretToDrains(ctx context.Context, obj client.Object) []reconcile.Request {
	var drains shpyrdv1.LogDrainList
	if err := r.List(ctx, &drains, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, d := range drains.Items {
		if d.Spec.HeadersFrom != nil && d.Spec.HeadersFrom.Name == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return out
}

// configMapToDrains requeues one drain (any) when the rendered ConfigMap is
// touched, so a manual edit is overwritten.
func (r *LogDrainReconciler) configMapToDrains(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != LogsNamespace || obj.GetName() != DrainsConfigMapName {
		return nil
	}
	var drains shpyrdv1.LogDrainList
	if err := r.List(ctx, &drains); err != nil || len(drains.Items) == 0 {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(&drains.Items[0])}}
}

// Reconcile re-renders the whole drain config (drains are cheap and few;
// rendering everything keeps a single source of truth), then updates the
// status of the drain that triggered the reconcile.
func (r *LogDrainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var all shpyrdv1.LogDrainList
	if err := r.List(ctx, &all); err != nil {
		return ctrl.Result{}, err
	}
	// Validate and resolve headers for every drain; invalid ones are left
	// out of the config and told so in their status.
	var rendered []renderedDrain
	invalid := map[string]string{}
	var envFrom []corev1.EnvFromSource
	wantMirrors := map[string]bool{}
	for i := range all.Items {
		d := &all.Items[i]
		rd, err := r.resolveDrain(ctx, d)
		if err != nil {
			invalid[client.ObjectKeyFromObject(d).String()] = err.Error()
			continue
		}
		rendered = append(rendered, rd)
		if len(rd.MirrorData) > 0 {
			name := mirrorSecretName(d)
			wantMirrors[name] = true
			if err := r.writeMirror(ctx, name, rd.MirrorData); err != nil {
				return ctrl.Result{}, err
			}
			envFrom = append(envFrom, corev1.EnvFromSource{
				Prefix:    envPrefix(d) + "_",
				SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Optional: ptrBool(true)},
			})
		}
	}
	if err := r.pruneMirrors(ctx, wantMirrors); err != nil {
		return ctrl.Result{}, err
	}
	// Only wire the agent when the logs-agent extension is installed.
	agentPresent := true
	ds := &appsv1.DaemonSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: LogsNamespace, Name: VectorDaemonSetName}, ds); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		agentPresent = false
	}
	if agentPresent {
		if err := r.writeConfig(ctx, renderDrains(rendered, r.SystemNamespace)); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.patchEnvFrom(ctx, ds, envFrom); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Status of the drain in this request.
	d := &shpyrdv1.LogDrain{}
	if err := r.Get(ctx, req.NamespacedName, d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := d.Status
	d.Status.ObservedGeneration = d.Generation
	switch {
	case !agentPresent:
		d.Status.Phase = shpyrdv1.DrainPending
		d.Status.Message = "the logs-agent extension is not enabled: shpyrd extensions enable logs-agent"
	case invalid[req.NamespacedName.String()] != "":
		d.Status.Phase = shpyrdv1.DrainFailing
		d.Status.Message = invalid[req.NamespacedName.String()]
	default:
		r.refreshDelivery(ctx, d)
	}
	if before.Phase != d.Status.Phase || before.Message != d.Status.Message || before.Sent != d.Status.Sent || before.Errors != d.Status.Errors {
		if err := r.Status().Update(ctx, d); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{RequeueAfter: drainStatusPeriod}, nil
}

// renderedDrain is a validated drain ready for rendering.
type renderedDrain struct {
	ID        string // Vector component id: drain_<ns>_<name>
	Namespace string
	Name      string
	URL       string
	Format    string
	Processes []string
	// Headers maps header names to the env var Vector expands.
	Headers map[string]string
	// MirrorData is the header Secret's data keyed by env-safe names, to be
	// copied into the agent's namespace.
	MirrorData map[string][]byte
	// Cluster drains receive every project.
	Cluster bool
}

// resolveDrain validates the spec and looks the header Secret up.
func (r *LogDrainReconciler) resolveDrain(ctx context.Context, d *shpyrdv1.LogDrain) (renderedDrain, error) {
	rd := renderedDrain{
		ID: componentID(d), Namespace: d.Namespace, Name: d.Name, URL: d.Spec.URL,
		Format: d.EffectiveFormat(), Processes: d.Spec.Processes,
		Cluster: d.Namespace == r.SystemNamespace, Headers: map[string]string{},
	}
	if err := ValidateDrainURL(d.Spec.URL, rd.Format); err != nil {
		return rd, err
	}
	if d.Spec.HeadersFrom != nil {
		sec := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: d.Spec.HeadersFrom.Name}, sec); err != nil {
			return rd, fmt.Errorf("headers secret %q: %w", d.Spec.HeadersFrom.Name, err)
		}
		// Header names become environment variable names: dashes and dots
		// are not allowed there, so the mirror Secret in the agent's
		// namespace uses sanitized keys and the config maps them back.
		rd.MirrorData = map[string][]byte{}
		for k, v := range sec.Data {
			env := strings.ToUpper(sanitize(k))
			rd.Headers[k] = envPrefix(d) + "_" + env
			rd.MirrorData[env] = v
		}
	}
	return rd, nil
}

// mirrorSecretName is the copy of a drain's header Secret in the agent's
// namespace (envFrom can only reference Secrets of the pod's namespace).
func mirrorSecretName(d *shpyrdv1.LogDrain) string {
	return "drain-" + sanitizeDNS(d.Namespace) + "-" + sanitizeDNS(d.Name) + "-headers"
}

func sanitizeDNS(s string) string { return strings.ToLower(strings.ReplaceAll(sanitize(s), "_", "-")) }

// LabelDrainHeaders marks header mirrors so stale ones can be pruned.
const LabelDrainHeaders = "shpyrd.io/drain-headers"

// ValidateDrainURL checks the receiver URL against the format.
func ValidateDrainURL(raw, format string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("url %q: need scheme://host[:port]", raw)
	}
	switch format {
	case shpyrdv1.DrainFormatJSON:
		if u.Scheme != "https" && u.Scheme != "http" {
			return fmt.Errorf("json drains need an http(s):// url, got %s://", u.Scheme)
		}
	case shpyrdv1.DrainFormatSyslog:
		if u.Scheme != "syslog" && u.Scheme != "syslog+tls" {
			return fmt.Errorf("syslog drains need a syslog:// or syslog+tls:// url, got %s://", u.Scheme)
		}
		if u.Port() == "" {
			return fmt.Errorf("syslog url %q needs a port (for example :6514)", raw)
		}
	default:
		return fmt.Errorf("format must be json or syslog, got %q", format)
	}
	return nil
}

// componentID is the Vector component id of a drain.
func componentID(d *shpyrdv1.LogDrain) string {
	return "drain_" + sanitize(d.Namespace) + "_" + sanitize(d.Name)
}

// envPrefix is the environment variable prefix of a drain's headers.
func envPrefix(d *shpyrdv1.LogDrain) string {
	return strings.ToUpper("DRAIN_" + sanitize(d.Namespace) + "_" + sanitize(d.Name))
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// renderDrains writes Vector's drains.yaml: a filter and a sink per drain.
// Deterministic (sorted) so the ConfigMap only changes when drains do.
func renderDrains(drains []renderedDrain, systemNS string) string {
	sort.Slice(drains, func(i, j int) bool { return drains[i].ID < drains[j].ID })
	var b strings.Builder
	b.WriteString("# Rendered by shpyrd from LogDrain objects (RFC-0023). Do not edit.\n")
	if len(drains) == 0 {
		b.WriteString("transforms: {}\nsinks: {}\n")
		return b.String()
	}
	b.WriteString("transforms:\n")
	for _, d := range drains {
		fmt.Fprintf(&b, "  %s:\n    type: filter\n    inputs: [shpyrd_enrich]\n    condition: %q\n", d.ID, drainCondition(d))
		if d.Format == shpyrdv1.DrainFormatSyslog {
			// RFC 5424: <PRI>1 TIMESTAMP HOSTNAME APP-NAME PROCID MSGID SD MSG,
			// with the project as APP-NAME and the instance as PROCID.
			fmt.Fprintf(&b, "  %s_fmt:\n    type: remap\n    inputs: [%s]\n    source: |\n", d.ID, d.ID)
			for _, line := range strings.Split(syslogVRL, "\n") {
				fmt.Fprintf(&b, "      %s\n", line)
			}
		}
	}
	b.WriteString("sinks:\n")
	for _, d := range drains {
		input := d.ID
		if d.Format == shpyrdv1.DrainFormatSyslog {
			input = d.ID + "_fmt"
		}
		fmt.Fprintf(&b, "  %s_sink:\n    inputs: [%s]\n", d.ID, input)
		switch d.Format {
		case shpyrdv1.DrainFormatSyslog:
			renderSyslogSink(&b, d)
		default:
			renderHTTPSink(&b, d)
		}
	}
	return b.String()
}

// syslogVRL composes an RFC 5424 line into .message from the enriched event.
const syslogVRL = `sev = 6
lvl = downcase(to_string(.level) ?? "")
if lvl == "error" || lvl == "fatal" || lvl == "panic" { sev = 3 }
if lvl == "warn" || lvl == "warning" { sev = 4 }
if lvl == "debug" || lvl == "trace" { sev = 7 }
pri = 8 + sev
ts = to_string(format_timestamp(.timestamp, "%+") ?? "-")
app = to_string(.project) ?? "-"
procid = to_string(.instance) ?? "-"
body = to_string(.msg) ?? to_string(.message) ?? ""
.message = "<" + to_string(pri) + ">1 " + ts + " shpyrd " + app + " " + procid + " - - " + body`

// drainCondition is the VRL condition selecting the drain's lines.
func drainCondition(d renderedDrain) string {
	var parts []string
	if !d.Cluster {
		parts = append(parts, fmt.Sprintf(".project == %q", project.FromNamespace(d.Namespace)))
	}
	if len(d.Processes) > 0 {
		quoted := make([]string, 0, len(d.Processes))
		for _, p := range d.Processes {
			quoted = append(quoted, fmt.Sprintf("%q", p))
		}
		parts = append(parts, fmt.Sprintf("includes([%s], .process)", strings.Join(quoted, ", ")))
	}
	if len(parts) == 0 {
		return "true"
	}
	return strings.Join(parts, " && ")
}

func renderHTTPSink(b *strings.Builder, d renderedDrain) {
	fmt.Fprintf(b, "    type: http\n    uri: %q\n    method: post\n", d.URL)
	b.WriteString("    encoding:\n      codec: json\n    framing:\n      method: newline_delimited\n")
	b.WriteString("    batch:\n      max_events: 500\n      timeout_secs: 2\n")
	b.WriteString("    request:\n      retry_attempts: 5\n")
	if len(d.Headers) > 0 {
		b.WriteString("      headers:\n")
		names := make([]string, 0, len(d.Headers))
		for k := range d.Headers {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(b, "        %q: \"${%s}\"\n", k, d.Headers[k])
		}
	}
	b.WriteString("    healthcheck:\n      enabled: false\n")
}

func renderSyslogSink(b *strings.Builder, d renderedDrain) {
	u, _ := url.Parse(d.URL)
	// The <id>_fmt remap composed the RFC 5424 line into .message; the
	// socket sink sends it verbatim, one line per event.
	fmt.Fprintf(b, "    type: socket\n    mode: tcp\n    address: %q\n", u.Host)
	if u.Scheme == "syslog+tls" {
		b.WriteString("    tls:\n      enabled: true\n")
	}
	b.WriteString("    encoding:\n      codec: raw_message\n    framing:\n      method: newline_delimited\n")
	b.WriteString("    healthcheck:\n      enabled: false\n")
}

// writeConfig stores drains.yaml and, for syslog drains, the formatting
// remap that composes RFC 5424 lines.
func (r *LogDrainReconciler) writeConfig(ctx context.Context, body string) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: DrainsConfigMapName, Namespace: LogsNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = mergeMaps(cm.Labels, map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"})
		cm.Data = map[string]string{"drains.yaml": body}
		return nil
	})
	if err != nil {
		return fmt.Errorf("write %s: %w", DrainsConfigMapName, err)
	}
	return nil
}

// writeMirror copies a drain's headers into the agent's namespace.
func (r *LogDrainReconciler) writeMirror(ctx context.Context, name string, data map[string][]byte) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: LogsNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Type = corev1.SecretTypeOpaque
		sec.Labels = mergeMaps(sec.Labels, map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", LabelDrainHeaders: "true"})
		sec.Data = data
		return nil
	})
	if err != nil {
		return fmt.Errorf("mirror headers %s: %w", name, err)
	}
	return nil
}

// pruneMirrors deletes header mirrors of drains that no longer exist.
func (r *LogDrainReconciler) pruneMirrors(ctx context.Context, want map[string]bool) error {
	var list corev1.SecretList
	if err := r.List(ctx, &list, client.InNamespace(LogsNamespace), client.MatchingLabels{LabelDrainHeaders: "true"}); err != nil {
		return client.IgnoreNotFound(err)
	}
	for i := range list.Items {
		if !want[list.Items[i].Name] {
			if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

// patchEnvFrom gives Vector the header Secrets as environment variables.
// Only the drain-owned entries (prefix DRAIN_) are managed; a change rolls
// the DaemonSet once per Secret set change.
func (r *LogDrainReconciler) patchEnvFrom(ctx context.Context, ds *appsv1.DaemonSet, want []corev1.EnvFromSource) error {
	if len(ds.Spec.Template.Spec.Containers) == 0 {
		return nil
	}
	c := &ds.Spec.Template.Spec.Containers[0]
	var kept []corev1.EnvFromSource
	for _, e := range c.EnvFrom {
		if !strings.HasPrefix(e.Prefix, "DRAIN_") {
			kept = append(kept, e)
		}
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Prefix < want[j].Prefix })
	merged := append(kept, want...)
	if equalEnvFrom(c.EnvFrom, merged) {
		return nil
	}
	c.EnvFrom = merged
	if err := r.Update(ctx, ds); err != nil {
		return fmt.Errorf("update vector env: %w", err)
	}
	return nil
}

func equalEnvFrom(a, b []corev1.EnvFromSource) bool {
	if len(a) != len(b) {
		return false
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// refreshDelivery reads Vector's metrics for the drain's sink and updates
// phase, counters and the last delivery time.
func (r *LogDrainReconciler) refreshDelivery(ctx context.Context, d *shpyrdv1.LogDrain) {
	if r.HTTP == nil {
		if d.Status.Phase == "" {
			d.Status.Phase = shpyrdv1.DrainPending
			d.Status.Message = "configured; waiting for the first lines"
		}
		return
	}
	sent, errs, ok := r.sinkCounters(ctx, componentID(d)+"_sink")
	if !ok {
		if d.Status.Phase == "" {
			d.Status.Phase = shpyrdv1.DrainPending
			d.Status.Message = "configured; waiting for the agent to load it"
		}
		return
	}
	switch {
	case sent > d.Status.Sent:
		d.Status.Phase = shpyrdv1.DrainActive
		now := metav1.Now()
		d.Status.LastDeliveryAt = &now
		d.Status.Message = fmt.Sprintf("%d lines delivered", sent)
	case errs > d.Status.Errors:
		d.Status.Phase = shpyrdv1.DrainFailing
		d.Status.Message = fmt.Sprintf("%d delivery errors, nothing sent since the last check", errs)
		if r.Recorder != nil {
			r.Recorder.Event(d, corev1.EventTypeWarning, "DrainFailing", d.Status.Message)
		}
	case d.Status.Phase == "":
		d.Status.Phase = shpyrdv1.DrainPending
		d.Status.Message = "configured; waiting for the first lines"
	}
	d.Status.Sent, d.Status.Errors = sent, errs
}

// sinkCounters scrapes Vector's Prometheus metrics for one component.
func (r *LogDrainReconciler) sinkCounters(ctx context.Context, component string) (sent, errs int64, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, VectorMetrics, nil)
	if err != nil {
		return 0, 0, false
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, 0, false
	}
	return parseSinkCounters(string(body), component)
}

// parseSinkCounters reads component_sent_events_total and
// component_errors_total for a component out of a Prometheus exposition.
func parseSinkCounters(metrics, component string) (sent, errs int64, ok bool) {
	needle := `component_id="` + component + `"`
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		// Exposition: name{labels} value [timestamp]; the value is the
		// first field after the closing brace.
		var v float64
		if i := strings.LastIndexByte(line, '}'); i > 0 {
			fields := strings.Fields(line[i+1:])
			if len(fields) > 0 {
				fmt.Sscanf(fields[0], "%g", &v)
			}
		}
		switch {
		case strings.HasPrefix(line, "vector_component_sent_events_total"):
			sent += int64(v)
			ok = true
		case strings.HasPrefix(line, "vector_component_errors_total"):
			errs += int64(v)
			ok = true
		}
	}
	return sent, errs, ok
}

func ptrBool(b bool) *bool { return &b }

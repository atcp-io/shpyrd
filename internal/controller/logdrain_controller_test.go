package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/kube"
)

// RFC-0023: drains render into Vector's config; header values never do.

func TestRenderDrains(t *testing.T) {
	out := renderDrains([]renderedDrain{
		{ID: "drain_app_shop_datadog", Namespace: "app-shop", Name: "datadog", URL: "https://http-intake.logs.datadoghq.com/api/v2/logs", Format: "json",
			Processes: []string{"web"}, Headers: map[string]string{"DD-API-KEY": "DRAIN_APP_SHOP_DATADOG_DD_API_KEY"}},
		{ID: "drain_shpyrd_system_siem", Namespace: "shpyrd-system", Name: "siem", URL: "syslog+tls://logs.example.com:6514", Format: "syslog", Cluster: true},
	}, "shpyrd-system")

	// Valid YAML with the expected components.
	var doc struct {
		Transforms map[string]map[string]any `json:"transforms"`
		Sinks      map[string]map[string]any `json:"sinks"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("rendered config is not YAML: %v\n%s", err, out)
	}
	// Project drain: filtered by project and process; header via env var only.
	dd := doc.Transforms["drain_app_shop_datadog"]
	if dd["type"] != "filter" || dd["condition"] != `.namespace == "app-shop" && includes(["web"], .process)` {
		t.Errorf("datadog filter = %v", dd)
	}
	sink := doc.Sinks["drain_app_shop_datadog_sink"]
	if sink["type"] != "http" || sink["uri"] != "https://http-intake.logs.datadoghq.com/api/v2/logs" {
		t.Errorf("datadog sink = %v", sink)
	}
	if !strings.Contains(out, `"DD-API-KEY": "${DRAIN_APP_SHOP_DATADOG_DD_API_KEY}"`) {
		t.Errorf("header must be an env var reference:\n%s", out)
	}
	// Cluster drain: no project condition; syslog gets a formatting remap
	// and a TLS socket sink fed by it.
	siem := doc.Transforms["drain_shpyrd_system_siem"]
	if siem["condition"] != "true" {
		t.Errorf("cluster drain condition = %v", siem["condition"])
	}
	fmtT := doc.Transforms["drain_shpyrd_system_siem_fmt"]
	if fmtT["type"] != "remap" || !strings.Contains(fmtT["source"].(string), `">1 "`) {
		t.Errorf("syslog remap = %v", fmtT)
	}
	ssink := doc.Sinks["drain_shpyrd_system_siem_sink"]
	if ssink["type"] != "socket" || ssink["address"] != "logs.example.com:6514" || ssink["tls"] == nil {
		t.Errorf("syslog sink = %v", ssink)
	}
	if inputs, _ := ssink["inputs"].([]any); len(inputs) != 1 || inputs[0] != "drain_shpyrd_system_siem_fmt" {
		t.Errorf("syslog sink must read the formatted stream: %v", ssink["inputs"])
	}
	// Deterministic.
	if again := renderDrains([]renderedDrain{
		{ID: "drain_shpyrd_system_siem", Namespace: "shpyrd-system", Name: "siem", URL: "syslog+tls://logs.example.com:6514", Format: "syslog", Cluster: true},
		{ID: "drain_app_shop_datadog", Namespace: "app-shop", Name: "datadog", URL: "https://http-intake.logs.datadoghq.com/api/v2/logs", Format: "json",
			Processes: []string{"web"}, Headers: map[string]string{"DD-API-KEY": "DRAIN_APP_SHOP_DATADOG_DD_API_KEY"}},
	}, "shpyrd-system"); again != out {
		t.Error("rendering must not depend on input order")
	}
	// Empty.
	if e := renderDrains(nil, "shpyrd-system"); !strings.Contains(e, "transforms: {}") || !strings.Contains(e, "sinks: {}") {
		t.Errorf("empty render = %q", e)
	}
}

func TestValidateDrainURL(t *testing.T) {
	good := map[string]string{
		"https://in.logs.example.com/ingest": "json",
		"http://echo.default.svc:8080/":      "json",
		"syslog://logs.example.com:514":      "syslog",
		"syslog+tls://logs.example.com:6514": "syslog",
	}
	for u, f := range good {
		if err := ValidateDrainURL(u, f); err != nil {
			t.Errorf("%s (%s): %v", u, f, err)
		}
	}
	bad := map[string]string{
		"ftp://x/":                     "json",
		"https://x/":                   "syslog",
		"syslog://logs.example.com":    "syslog", // no port
		"not a url":                    "json",
		"https://in.logs.example.com/": "csv",
	}
	for u, f := range bad {
		if err := ValidateDrainURL(u, f); err == nil {
			t.Errorf("%s (%s) accepted", u, f)
		}
	}
}

func TestParseSinkCounters(t *testing.T) {
	metrics := `# HELP vector_component_sent_events_total x
vector_component_sent_events_total{component_id="drain_app_shop_dd_sink",component_kind="sink"} 1234 1790194705401
vector_component_errors_total{component_id="drain_app_shop_dd_sink",component_kind="sink",error_type="request_failed"} 2
vector_component_sent_events_total{component_id="other_sink",component_kind="sink"} 99
`
	sent, errs, ok := parseSinkCounters(metrics, "drain_app_shop_dd_sink")
	if !ok || sent != 1234 || errs != 2 {
		t.Errorf("counters = %d %d %v", sent, errs, ok)
	}
	if _, _, ok := parseSinkCounters(metrics, "missing_sink"); ok {
		t.Error("missing component must not be ok")
	}
}

func newDrainReconciler(t *testing.T, objs ...client.Object) (*LogDrainReconciler, client.Client) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&shpyrdv1.LogDrain{}).Build()
	return &LogDrainReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10), SystemNamespace: "shpyrd-system"}, c
}

func vectorDaemonSet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: VectorDaemonSetName, Namespace: LogsNamespace},
		Spec:       appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vector"}}}}},
	}
}

func TestLogDrainReconcile(t *testing.T) {
	ctx := context.Background()
	drain := &shpyrdv1.LogDrain{
		ObjectMeta: metav1.ObjectMeta{Name: "dd", Namespace: "app-shop"},
		Spec:       shpyrdv1.LogDrainSpec{URL: "https://intake.example.com/v1", HeadersFrom: &corev1.LocalObjectReference{Name: "dd-headers"}},
	}
	headers := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "dd-headers", Namespace: "app-shop"}, Data: map[string][]byte{"DD-API-KEY": []byte("s3cret")}}
	cluster := &shpyrdv1.LogDrain{
		ObjectMeta: metav1.ObjectMeta{Name: "siem", Namespace: "shpyrd-system"},
		Spec:       shpyrdv1.LogDrainSpec{URL: "syslog://siem.example.com:514"},
	}
	r, c := newDrainReconciler(t, drain, headers, cluster, vectorDaemonSet())
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(drain)}); err != nil {
		t.Fatal(err)
	}
	// The rendered ConfigMap has both drains and no secret value.
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: LogsNamespace, Name: DrainsConfigMapName}, cm); err != nil {
		t.Fatal(err)
	}
	body := cm.Data["drains.yaml"]
	if !strings.Contains(body, "drain_app_shop_dd_sink") || !strings.Contains(body, "drain_shpyrd_system_siem_sink") {
		t.Errorf("config lacks drains:\n%s", body)
	}
	if strings.Contains(body, "s3cret") {
		t.Error("secret value leaked into the config")
	}
	if !strings.Contains(body, `"DD-API-KEY": "${DRAIN_APP_SHOP_DD_DD_API_KEY}"`) {
		t.Errorf("header env reference must use an env-safe name:\n%s", body)
	}
	// The headers are mirrored into the agent's namespace under env-safe
	// keys, and the DaemonSet references the mirror with the drain's prefix.
	mirror := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: LogsNamespace, Name: "drain-app-shop-dd-headers"}, mirror); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if string(mirror.Data["DD_API_KEY"]) != "s3cret" {
		t.Errorf("mirror data = %v", mirror.Data)
	}
	ds := &appsv1.DaemonSet{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: LogsNamespace, Name: VectorDaemonSetName}, ds)
	ef := ds.Spec.Template.Spec.Containers[0].EnvFrom
	if len(ef) != 1 || ef[0].Prefix != "DRAIN_APP_SHOP_DD_" || ef[0].SecretRef.Name != "drain-app-shop-dd-headers" {
		t.Errorf("envFrom = %+v", ef)
	}
	// Status: pending until lines flow (no HTTP client in tests).
	got := &shpyrdv1.LogDrain{}
	_ = c.Get(ctx, client.ObjectKeyFromObject(drain), got)
	if got.Status.Phase != shpyrdv1.DrainPending {
		t.Errorf("status = %+v", got.Status)
	}

	// An invalid drain is reported and left out of the config.
	badDrain := &shpyrdv1.LogDrain{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "app-shop"}, Spec: shpyrdv1.LogDrainSpec{URL: "ftp://nope/"}}
	if err := c.Create(ctx, badDrain); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(badDrain)}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(badDrain), badDrain)
	if badDrain.Status.Phase != shpyrdv1.DrainFailing || !strings.Contains(badDrain.Status.Message, "http(s)://") {
		t.Errorf("bad drain status = %+v", badDrain.Status)
	}
	_ = c.Get(ctx, types.NamespacedName{Namespace: LogsNamespace, Name: DrainsConfigMapName}, cm)
	if strings.Contains(cm.Data["drains.yaml"], "drain_app_shop_bad") {
		t.Error("invalid drain must not be rendered")
	}

	// Removing a drain removes it from the config and its env.
	if err := c.Delete(ctx, drain); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, types.NamespacedName{Namespace: LogsNamespace, Name: DrainsConfigMapName}, cm)
	if strings.Contains(cm.Data["drains.yaml"], "drain_app_shop_dd") {
		t.Error("deleted drain still rendered")
	}
	_ = c.Get(ctx, types.NamespacedName{Namespace: LogsNamespace, Name: VectorDaemonSetName}, ds)
	if len(ds.Spec.Template.Spec.Containers[0].EnvFrom) != 0 {
		t.Errorf("envFrom after delete = %+v", ds.Spec.Template.Spec.Containers[0].EnvFrom)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: LogsNamespace, Name: "drain-app-shop-dd-headers"}, mirror); err == nil {
		t.Error("mirror must be pruned with its drain")
	}
}

func TestLogDrainWithoutAgent(t *testing.T) {
	drain := &shpyrdv1.LogDrain{ObjectMeta: metav1.ObjectMeta{Name: "dd", Namespace: "app-shop"}, Spec: shpyrdv1.LogDrainSpec{URL: "https://x.example.com/"}}
	r, c := newDrainReconciler(t, drain)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(drain)}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(drain), drain)
	if drain.Status.Phase != shpyrdv1.DrainPending || !strings.Contains(drain.Status.Message, "logs-agent") {
		t.Errorf("status without agent = %+v", drain.Status)
	}
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"time"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
)

const testToken = "secret-token"

func sampleApp(name string, phase string, releases ...shpyrdv1.Release) *shpyrdv1.App {
	return &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "app-" + name, CreationTimestamp: metav1.Now()},
		Spec:       shpyrdv1.AppSpec{Source: &shpyrdv1.Source{Git: &shpyrdv1.GitSource{URL: "https://example.test/" + name}}},
		Status: shpyrdv1.AppStatus{
			Phase:    phase,
			URL:      "https://" + name + ".example.test:8443",
			Image:    "10.96.0.50:5000/apps/" + name + "@sha256:aaaa",
			Releases: releases,
		},
	}
}

func newTestServer(t *testing.T, prom *PromClient, crObjs []client.Object, kubeObjs ...interface{}) (*Server, client.Client) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	cr := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(crObjs...).WithStatusSubresource(&shpyrdv1.App{}).Build()
	var runtimeObjs []runtimeObject
	for _, o := range kubeObjs {
		runtimeObjs = append(runtimeObjs, o.(runtimeObject))
	}
	cs := kubefake.NewSimpleClientset(toRuntime(runtimeObjs)...)
	k := &kube.Client{Kube: cs, Namespace: "shpyrd-system"}
	s := newServer(k, Options{Token: testToken, Apps: cr, Prometheus: prom, Public: PublicConfig{Domain: "example.test"}}, nil)
	return s, cr
}

func do(t *testing.T, s *Server, method, path, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAuth(t *testing.T) {
	s, _ := newTestServer(t, nil, nil)
	if rec := do(t, s, "GET", "/api/apps", "", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d want 401", rec.Code)
	}
	if rec := do(t, s, "GET", "/api/apps", "", true); rec.Code != http.StatusOK {
		t.Errorf("bearer: got %d want 200: %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("GET", "/api/apps", nil)
	req.Header.Set("X-Shpyrd-Token", testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("X-Shpyrd-Token: got %d want 200", rec.Code)
	}
	rec = do(t, s, "GET", "/api/config", "", false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"authRequired":true`) {
		t.Errorf("config must be public and report authRequired: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/api/healthz", "", false); rec.Code != http.StatusOK {
		t.Errorf("healthz must be public: %d", rec.Code)
	}
}

func TestListAndGetApps(t *testing.T) {
	s, _ := newTestServer(t, nil, []client.Object{
		sampleApp("zeta", shpyrdv1.PhaseRunning, shpyrdv1.Release{Number: 3, Image: "x"}),
		sampleApp("alpha", ""),
	})
	rec := do(t, s, "GET", "/api/apps", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list []AppSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Release != 3 || list[0].Phase != shpyrdv1.PhasePending {
		t.Errorf("unexpected list: %+v", list)
	}
	if rec := do(t, s, "GET", "/api/apps/app-zeta/zeta", "", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"phase":"Running"`) {
		t.Errorf("get: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/api/apps/app-nope/nope", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("missing app: %d", rec.Code)
	}
}

func TestScaleAndRollback(t *testing.T) {
	app := sampleApp("web1", shpyrdv1.PhaseRunning,
		shpyrdv1.Release{Number: 1, Image: "img-a"},
		shpyrdv1.Release{Number: 2, Image: "img-b"},
	)
	s, cr := newTestServer(t, nil, []client.Object{app})

	rec := do(t, s, "POST", "/api/apps/app-web1/web1/scale", `{"process":"web","replicas":3}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("scale: %d %s", rec.Code, rec.Body.String())
	}
	got := &shpyrdv1.App{}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1"}, got); err != nil {
		t.Fatal(err)
	}
	if r := got.Spec.Processes["web"].Replicas; r == nil || *r != 3 {
		t.Errorf("replicas not applied: %+v", got.Spec.Processes)
	}
	if rec := do(t, s, "POST", "/api/apps/app-web1/web1/scale", `{"process":"nope","replicas":1}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown process: %d", rec.Code)
	}

	rec = do(t, s, "POST", "/api/apps/app-web1/web1/rollback", `{"release":1}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback: %d %s", rec.Code, rec.Body.String())
	}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Image != "img-a" || got.Annotations[shpyrdv1.AnnotationReleaseNote] != "Rollback to v1" {
		t.Errorf("rollback not applied: image=%q annotations=%v", got.Spec.Image, got.Annotations)
	}
	if rec := do(t, s, "POST", "/api/apps/app-web1/web1/rollback", `{"release":9}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown release: %d", rec.Code)
	}
}

func TestMetrics(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if !strings.Contains(q, `web1\\.example\\.test`) && !strings.Contains(q, `namespace="app-web1"`) {
			t.Errorf("unexpected query %q", q)
		}
		switch {
		case strings.Contains(q, "sum by (class)"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"class":"2xx"},"values":[[1700000000,"1.5"],[1700000060,"2"]]},
				{"metric":{"class":"5xx"},"values":[[1700000000,"0.1"],[1700000060,"NaN"]]}]}}`))
		case strings.Contains(q, "label_shpyrd_io_process") && strings.Contains(q, "cpu"):
			// per-process query has no data yet -> fallback path
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"1.5"],[1700000060,"NaN"],[1700000120,"2"]]}]}}`))
		}
	}))
	defer prom.Close()
	app := sampleApp("web1", shpyrdv1.PhaseRunning, shpyrdv1.Release{Number: 1, Image: "x", CreatedAt: metav1.Now()})
	s, _ := newTestServer(t, NewPromClient(prom.URL), []client.Object{app})

	rec := do(t, s, "GET", "/api/apps/app-web1/web1/metrics?range=6h", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: %d %s", rec.Code, rec.Body.String())
	}
	var resp MetricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Range != "6h" || resp.Step != 360 || len(resp.Releases) != 1 || resp.Releases[0].Label != "v1" {
		t.Errorf("range/step/releases: %+v", resp)
	}
	byID := map[string]Chart{}
	for _, ch := range resp.Charts {
		if ch.Error != "" {
			t.Errorf("chart %s error: %s", ch.ID, ch.Error)
		}
		byID[ch.ID] = ch
	}
	if th := byID["throughput"]; len(th.Series) != 2 || th.Series[0].Name != "2xx" || len(th.Series[1].Points) != 1 {
		t.Errorf("throughput series = %+v (NaN must be dropped, series sorted)", th.Series)
	}
	if lat := byID["latency"]; len(lat.Series) != 3 || lat.Series[0].Name != "p50" || len(lat.Series[0].Points) != 2 {
		t.Errorf("latency series = %+v", lat.Series)
	}
	if cpu := byID["cpu"]; len(cpu.Series) != 1 || cpu.Series[0].Name != "all" {
		t.Errorf("cpu fallback series = %+v", cpu.Series)
	}
	if rec := do(t, s, "GET", "/api/apps/app-web1/web1/metrics?range=2h", "", true); rec.Code != http.StatusBadRequest {
		t.Errorf("bad range: %d", rec.Code)
	}
}

func TestSecretsAndCreateDeploy(t *testing.T) {
	s, cr := newTestServer(t, nil, []client.Object{sampleApp("web1", shpyrdv1.PhaseRunning)})

	rec := do(t, s, "PUT", "/api/apps/app-web1/web1/secrets", `{"set":{"A":"1"},"dotenv":"B=two\n# comment\nexport C='three'\n"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("set secrets: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "three") || strings.Contains(rec.Body.String(), `"1"`) {
		t.Errorf("values must never be returned: %s", rec.Body.String())
	}
	var resp ConfigVarsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Vars) != 3 || resp.Vars[0].Name != "A" || resp.Vars[2].UpdatedAt == "" {
		t.Errorf("vars = %+v", resp.Vars)
	}
	sec := &corev1.Secret{}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1-env"}, sec); err != nil {
		t.Fatal(err)
	}
	if string(sec.Data["C"]) != "three" || string(sec.Data["B"]) != "two" {
		t.Errorf("secret data = %v", sec.Data)
	}
	rec = do(t, s, "PUT", "/api/apps/app-web1/web1/secrets", `{"unset":["A","B"]}`, true)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || len(resp.Vars) != 1 || resp.Vars[0].Name != "C" {
		t.Errorf("unset: %d %+v", rec.Code, resp.Vars)
	}
	if rec := do(t, s, "PUT", "/api/apps/app-web1/web1/secrets", `{"set":{"1BAD":"x"}}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid key: %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/api/apps/app-web1/web1/secrets", "", true); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "three") {
		t.Errorf("list must not leak values: %d %s", rec.Code, rec.Body.String())
	}

	// Create + deploy from git through the API.
	rec = do(t, s, "POST", "/api/apps", `{"name":"newapp","processes":{"web":{},"worker":{}}}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/apps", `{"name":"Bad Name"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/apps", `{"name":"newapp"}`, true); rec.Code != http.StatusConflict {
		t.Errorf("duplicate: %d", rec.Code)
	}
	rec = do(t, s, "POST", "/api/apps/app-newapp/newapp/deploy", `{"git":{"url":"https://example.test/r"},"subPath":"svc"}`, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body.String())
	}
	got := &shpyrdv1.App{}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-newapp", Name: "newapp"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Source == nil || got.Spec.Source.Git.Revision != "main" || got.Spec.Source.SubPath != "svc" || len(got.Spec.Processes) != 2 {
		t.Errorf("deploy not applied: %+v", got.Spec)
	}
	ns := &corev1.Namespace{}
	if err := cr.Get(context.Background(), types.NamespacedName{Name: "app-newapp"}, ns); err != nil {
		t.Errorf("namespace not created: %v", err)
	}
}

func TestInstanceNamesAndDigest(t *testing.T) {
	mk := func(name, proc string, sec int) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{shpyrdv1.LabelProcess: proc},
			CreationTimestamp: metav1.NewTime(time.Unix(int64(1700000000+sec), 0)),
		}}
	}
	names := InstanceNames([]corev1.Pod{mk("w-b", "web", 20), mk("w-a", "web", 10), mk("k-a", "worker", 5)})
	if names["w-a"] != "web.1" || names["w-b"] != "web.2" || names["k-a"] != "worker.1" {
		t.Errorf("instance names = %v", names)
	}
	if got := Digest("10.96.0.50:5000/apps/x@sha256:a179fba6482cb8c1cad0100c425f0d1d0061e725d9bdb2c108fdeb0b72c7a65f"); got != "a179fba6482c" {
		t.Errorf("digest = %q", got)
	}
	if got := Digest("ghcr.io/o/r:tag"); got != "" {
		t.Errorf("digest of tag = %q", got)
	}
	ts, msg := splitTimestamp("2026-09-22T00:37:06.123456789Z GET / from 1.2.3.4")
	if ts == "" || msg != "GET / from 1.2.3.4" {
		t.Errorf("splitTimestamp = %q %q", ts, msg)
	}
}

func TestHostRegex(t *testing.T) {
	app := &shpyrdv1.App{Spec: shpyrdv1.AppSpec{Domains: []string{"a.example.test", "b.example.test"}}}
	if got := hostRegex(app); got != `a\\.example\\.test|b\\.example\\.test` {
		t.Errorf("hostRegex = %q", got)
	}
	app = &shpyrdv1.App{Status: shpyrdv1.AppStatus{URL: "https://c.example.test:8443"}}
	if got := hostRegex(app); got != `c\\.example\\.test` {
		t.Errorf("hostRegex from URL = %q", got)
	}
}

func TestClusterSummaryAndLogs(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:   corev1.NodeSystemInfo{Architecture: "arm64", KubeletVersion: "v1.37.0"},
		},
	}
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: install.InstallRecordName, Namespace: install.DefaultSystemNamespace},
		Data: map[string]string{
			"profile": "local", "version": "test", "vars": "SHPYRD_DOMAIN: example.test\n",
			"component.cert-manager": "name: cert-manager\nversion: v1.21.2\nappliedAt: now\n",
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web1-web-abc", Namespace: "app-web1", Labels: map[string]string{shpyrdv1.LabelApp: "web1", shpyrdv1.LabelProcess: "web"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	s, _ := newTestServer(t, nil, []client.Object{sampleApp("web1", shpyrdv1.PhaseRunning)}, node, record, pod)

	rec := do(t, s, "GET", "/api/cluster", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("cluster: %d %s", rec.Code, rec.Body.String())
	}
	var sum ClusterSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Install == nil || sum.Install.Domain != "example.test" || len(sum.Components) != 1 || sum.Components[0].Version != "v1.21.2" {
		t.Errorf("install/components: %+v", sum)
	}
	if len(sum.Nodes) != 1 || !sum.Nodes[0].Ready || sum.Nodes[0].Roles != "control-plane" || sum.Apps != 1 || sum.Phases["Running"] != 1 {
		t.Errorf("nodes/apps: %+v", sum)
	}

	rec = do(t, s, "GET", "/api/apps/app-web1/web1/logs?tail=10", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), " web.1 | ") {
		t.Errorf("logs: %d %q", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "GET", "/api/apps/app-web1/web1/logs?tail=10&format=json", "", true)
	var line LogLine
	if err := json.Unmarshal([]byte(strings.SplitN(rec.Body.String(), "\n", 2)[0]), &line); err != nil || line.Instance != "web.1" || line.Pod != "web1-web-abc" {
		t.Errorf("ndjson logs: %d %q (%v)", rec.Code, rec.Body.String(), err)
	}
}

func TestClusterMetrics(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.HasSuffix(r.URL.Path, "/query_range") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"nodename":"n1"},"values":[[1700000000,"12.5"]]}]}}`))
			return
		}
		label, val := "node", "1"
		switch {
		case strings.Contains(q, "node_cpu_seconds_total"):
			label, val = "nodename", "25"
		case strings.Contains(q, "MemAvailable"):
			label, val = "nodename", "50"
		case strings.Contains(q, `resource="cpu"`) && strings.Contains(q, "allocatable") && !strings.Contains(q, "requests"):
			val = "8"
		case strings.Contains(q, `resource="memory"`) && strings.Contains(q, "allocatable") && !strings.Contains(q, "requests"):
			val = "16000000000"
		case strings.Contains(q, `resource="pods"`):
			val = "110"
		case strings.Contains(q, "kube_pod_info"):
			val = "23"
		case strings.Contains(q, "requests"):
			val = "10"
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"` + label + `":"n1"},"value":[1700000000,"` + val + `"]}]}}`))
	}))
	defer prom.Close()
	s, _ := newTestServer(t, NewPromClient(prom.URL), nil)
	rec := do(t, s, "GET", "/api/cluster/metrics?range=1h", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("cluster metrics: %d %s", rec.Code, rec.Body.String())
	}
	var cm ClusterMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.Nodes) != 1 || cm.Nodes[0].CPUUsedPct != 25 || cm.Nodes[0].MemoryUsedPct != 50 || cm.Nodes[0].Pods != 23 || cm.Nodes[0].PodCapacity != 110 {
		t.Errorf("node usage = %+v", cm.Nodes)
	}
	if cm.Total.CPUUsedPct != 25 || cm.Total.CPUCores != 8 || cm.Total.CPURequestedPct != 10 {
		t.Errorf("total = %+v", cm.Total)
	}
	if len(cm.Charts) != 2 || len(cm.Charts[0].Series) != 1 || cm.Charts[0].Series[0].Name != "n1" {
		t.Errorf("charts = %+v", cm.Charts)
	}
}

func TestSizesAndResize(t *testing.T) {
	s, cr := newTestServer(t, nil, []client.Object{sampleApp("web1", shpyrdv1.PhaseRunning)})
	rec := do(t, s, "GET", "/api/sizes", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"default":"shared-s"`) {
		t.Fatalf("defaults: %d %s", rec.Code, rec.Body.String())
	}
	body := `{"default":"tiny","sizes":[{"name":"tiny","kind":"shared","cpu":"0.1","memory":"32Mi"},{"name":"big","kind":"dedicated","cpu":"4","memory":"8Gi"}]}`
	if rec := do(t, s, "PUT", "/api/sizes", body, true); rec.Code != http.StatusOK {
		t.Fatalf("put sizes: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "PUT", "/api/sizes", `{"default":"nope","sizes":[{"name":"a","kind":"shared","cpu":"1","memory":"1Gi"}]}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid catalog: %d", rec.Code)
	}
	rec = do(t, s, "GET", "/api/sizes", "", true)
	if !strings.Contains(rec.Body.String(), `"default":"tiny"`) || !strings.Contains(rec.Body.String(), `"big"`) {
		t.Errorf("catalog not saved: %s", rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/apps/app-web1/web1/resize", `{"process":"web","size":"big"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("resize: %d %s", rec.Code, rec.Body.String())
	}
	got := &shpyrdv1.App{}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Processes["web"].Size != "big" {
		t.Errorf("size not applied: %+v", got.Spec.Processes)
	}
	if rec := do(t, s, "POST", "/api/apps/app-web1/web1/resize", `{"process":"web","size":"nope"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown size: %d", rec.Code)
	}
}

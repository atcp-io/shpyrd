package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"time"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/ext/all"
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
	cr := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(crObjs...).WithStatusSubresource(&shpyrdv1.App{}, &shpyrdv1.Volume{}).Build()
	var runtimeObjs []runtimeObject
	for _, o := range kubeObjs {
		runtimeObjs = append(runtimeObjs, o.(runtimeObject))
	}
	cs := kubefake.NewSimpleClientset(toRuntime(runtimeObjs)...)
	k := &kube.Client{Kube: cs, Namespace: "shpyrd-system"}
	s, err := newServer(k, Options{Token: testToken, Apps: cr, Prometheus: prom, Public: PublicConfig{Domain: "example.test"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
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
	if rec := do(t, s, "GET", "/api/projects", "", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d want 401", rec.Code)
	}
	if rec := do(t, s, "GET", "/api/projects", "", true); rec.Code != http.StatusOK {
		t.Errorf("bearer: got %d want 200: %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("GET", "/api/projects", nil)
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
	rec := do(t, s, "GET", "/api/projects", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list []AppSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Slug != "alpha" || list[0].DisplayName != "alpha" || list[1].Release != 3 || list[0].Phase != shpyrdv1.PhasePending {
		t.Errorf("unexpected list: %+v", list)
	}
	if rec := do(t, s, "GET", "/api/projects/zeta", "", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"phase":"Running"`) {
		t.Errorf("get: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/api/projects/nope", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("missing app: %d", rec.Code)
	}
}

func TestScaleAndRollback(t *testing.T) {
	app := sampleApp("web1", shpyrdv1.PhaseRunning,
		shpyrdv1.Release{Number: 1, Image: "img-a"},
		shpyrdv1.Release{Number: 2, Image: "img-b"},
	)
	s, cr := newTestServer(t, nil, []client.Object{app})

	rec := do(t, s, "POST", "/api/projects/web1/scale", `{"process":"web","replicas":3}`, true)
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
	if rec := do(t, s, "POST", "/api/projects/web1/scale", `{"process":"nope","replicas":1}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown process: %d", rec.Code)
	}

	rec = do(t, s, "POST", "/api/projects/web1/rollback", `{"release":1}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback: %d %s", rec.Code, rec.Body.String())
	}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Image != "img-a" || got.Annotations[shpyrdv1.AnnotationReleaseNote] != "Rollback to v1" {
		t.Errorf("rollback not applied: image=%q annotations=%v", got.Spec.Image, got.Annotations)
	}
	if rec := do(t, s, "POST", "/api/projects/web1/rollback", `{"release":9}`, true); rec.Code != http.StatusBadRequest {
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

	rec := do(t, s, "GET", "/api/projects/web1/metrics?range=6h", "", true)
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
	if rec := do(t, s, "GET", "/api/projects/web1/metrics?range=2h", "", true); rec.Code != http.StatusBadRequest {
		t.Errorf("bad range: %d", rec.Code)
	}
}

func TestSecretsAndCreateDeploy(t *testing.T) {
	s, cr := newTestServer(t, nil, []client.Object{sampleApp("web1", shpyrdv1.PhaseRunning)})

	rec := do(t, s, "PUT", "/api/projects/web1/secrets", `{"set":{"A":"1"},"dotenv":"B=two\n# comment\nexport C='three'\n"}`, true)
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
	rec = do(t, s, "PUT", "/api/projects/web1/secrets", `{"unset":["A","B"]}`, true)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || len(resp.Vars) != 1 || resp.Vars[0].Name != "C" {
		t.Errorf("unset: %d %+v", rec.Code, resp.Vars)
	}
	if rec := do(t, s, "PUT", "/api/projects/web1/secrets", `{"set":{"1BAD":"x"}}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid key: %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/api/projects/web1/secrets", "", true); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "three") {
		t.Errorf("list must not leak values: %d %s", rec.Code, rec.Body.String())
	}

	// Create + deploy from git through the API.
	rec = do(t, s, "POST", "/api/projects", `{"name":"newapp","processes":{"web":{},"worker":{}}}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects", `{"name":"!!!"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("name without letters or digits: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects", `{"name":"x","slug":"Bad Slug"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("bad explicit slug: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects", `{"name":"newapp"}`, true); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "newapp-2") {
		t.Errorf("duplicate: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "POST", "/api/projects/newapp/deploy", `{"git":{"url":"https://example.test/r"},"subPath":"svc"}`, true)
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
	if got.Spec.Build != nil && got.Spec.Build.Strategy != "" {
		t.Errorf("no strategy requested must keep the app's setting: %+v", got.Spec.Build)
	}
	rec = do(t, s, "POST", "/api/projects/newapp/deploy", `{"git":{"url":"https://example.test/r"},"strategy":"dockerfile","dockerfile":"deploy/Dockerfile"}`, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("dockerfile deploy: %d %s", rec.Code, rec.Body.String())
	}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-newapp", Name: "newapp"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Build == nil || got.Spec.Build.Strategy != shpyrdv1.StrategyDockerfile || got.Spec.Build.Dockerfile != "deploy/Dockerfile" {
		t.Errorf("dockerfile strategy not applied: %+v", got.Spec.Build)
	}
	if rec := do(t, s, "POST", "/api/projects/newapp/deploy", `{"git":{"url":"https://example.test/r"},"strategy":"magic"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown strategy: %d", rec.Code)
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
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{
			"node-role.kubernetes.io/control-plane": "",
			"node.kubernetes.io/instance-type":      "VM.Standard.E5.Flex",
			"topology.kubernetes.io/zone":           "SA-SAOPAULO-1-AD-1",
		}},
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
	if n := sum.Nodes[0]; n.InstanceType != "VM.Standard.E5.Flex" || n.Zone != "SA-SAOPAULO-1-AD-1" {
		t.Errorf("node topology: %+v", n)
	}

	rec = do(t, s, "GET", "/api/projects/web1/logs?tail=10", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), " web.1 | ") {
		t.Errorf("logs: %d %q", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "GET", "/api/projects/web1/logs?tail=10&format=json", "", true)
	var line LogLine
	if err := json.Unmarshal([]byte(strings.SplitN(rec.Body.String(), "\n", 2)[0]), &line); err != nil || line.Instance != "web.1" || line.Pod != "web1-web-abc" {
		t.Errorf("ndjson logs: %d %q (%v)", rec.Code, rec.Body.String(), err)
	}
}

func TestNodeInfoRoles(t *testing.T) {
	mk := func(labels map[string]string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: labels}}
	}
	cases := []struct {
		labels map[string]string
		roles  string
	}{
		{nil, "worker"},
		{map[string]string{"node-role.kubernetes.io/node": ""}, "worker"},
		{map[string]string{"node-role.kubernetes.io/control-plane": "", "node-role.kubernetes.io/master": ""}, "control-plane,master"},
		{map[string]string{"beta.kubernetes.io/instance-type": "t3.large"}, "worker"},
	}
	for _, tc := range cases {
		if got := nodeInfo(mk(tc.labels)).Roles; got != tc.roles {
			t.Errorf("roles(%v) = %q, want %q", tc.labels, got, tc.roles)
		}
	}
	if got := nodeInfo(mk(map[string]string{"beta.kubernetes.io/instance-type": "t3.large"})).InstanceType; got != "t3.large" {
		t.Errorf("beta instance-type label = %q", got)
	}
}

func TestClusterMetrics(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.HasSuffix(r.URL.Path, "/query_range") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"node":"n1"},"values":[[1700000000,"12.5"]]}]}}`))
			return
		}
		label, val := "node", "1"
		switch {
		case strings.Contains(q, "node_cpu_seconds_total"):
			val = "25"
		case strings.Contains(q, "MemAvailable"):
			val = "50"
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
	if rec := do(t, s, "POST", "/api/projects/web1/resize", `{"process":"web","size":"big"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("resize: %d %s", rec.Code, rec.Body.String())
	}
	got := &shpyrdv1.App{}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Processes["web"].Size != "big" {
		t.Errorf("size not applied: %+v", got.Spec.Processes)
	}
	if rec := do(t, s, "POST", "/api/projects/web1/resize", `{"process":"web","size":"nope"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown size: %d", rec.Code)
	}
}

func TestApplyProcesses(t *testing.T) {
	s, cr := newTestServer(t, nil, []client.Object{sampleApp("web1", shpyrdv1.PhaseRunning)})
	rec := do(t, s, "POST", "/api/projects/web1/processes", `{"processes":{"web":{"size":"shared-m","replicas":3},"worker":{"size":"shared-xs"}}}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown process worker must fail: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "POST", "/api/projects/web1/processes", `{"processes":{"web":{"size":"shared-m","replicas":3}}}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	got := &shpyrdv1.App{}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-web1", Name: "web1"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Processes["web"].Size != "shared-m" || *got.Spec.Processes["web"].Replicas != 3 {
		t.Errorf("changes not applied: %+v", got.Spec.Processes["web"])
	}
	if rec := do(t, s, "POST", "/api/projects/web1/processes", `{"processes":{"web":{"size":"nope"}}}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown size: %d", rec.Code)
	}
}

func TestVolumesAPIAndScaleRefusal(t *testing.T) {
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "app-demo"},
		Spec: shpyrdv1.AppSpec{Processes: map[string]shpyrdv1.Process{
			"web": {Volumes: []shpyrdv1.VolumeMount{{Name: "data", Path: "/data"}}},
		}},
	}
	s, cr := newTestServer(t, nil, []client.Object{app})

	if rec := do(t, s, "GET", "/api/projects/demo/volumes", "", true); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty list: %d %s", rec.Code, rec.Body.String())
	}
	rec := do(t, s, "POST", "/api/projects/demo/volumes", `{"name":"data","size":"5Gi"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var view VolumeView
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Size != "5Gi" || view.Shared || view.Phase != "Pending" || view.MountedBy == nil {
		t.Errorf("view = %+v", view)
	}
	if rec := do(t, s, "POST", "/api/projects/demo/volumes", `{"name":"data","size":"5Gi"}`, true); rec.Code != http.StatusConflict {
		t.Errorf("duplicate: %d", rec.Code)
	}
	for _, bad := range []string{`{"name":"Data","size":"5Gi"}`, `{"name":"x","size":"five"}`, `{"name":"x","size":"-1Gi"}`} {
		if rec := do(t, s, "POST", "/api/projects/demo/volumes", bad, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	if rec := do(t, s, "POST", "/api/projects/demo/volumes", `{"name":"assets","size":"1Gi","shared":true,"storageClass":"nfs"}`, true); rec.Code != http.StatusCreated {
		t.Errorf("shared create: %d %s", rec.Code, rec.Body.String())
	}

	// Resize: grow ok, shrink refused.
	if rec := do(t, s, "PUT", "/api/projects/demo/volumes/data", `{"size":"10Gi"}`, true); rec.Code != http.StatusOK {
		t.Errorf("grow: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "PUT", "/api/projects/demo/volumes/data", `{"size":"1Gi"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("shrink: %d", rec.Code)
	}
	if rec := do(t, s, "PUT", "/api/projects/demo/volumes/nope", `{"size":"1Gi"}`, true); rec.Code != http.StatusNotFound {
		t.Errorf("missing: %d", rec.Code)
	}

	// Scaling web past 1 is refused because "data" is single-instance.
	if rec := do(t, s, "POST", "/api/projects/demo/scale", `{"process":"web","replicas":3}`, true); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "single-instance volume") {
		t.Errorf("scale: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/demo/processes", `{"processes":{"web":{"replicas":2}}}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("batch scale: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/demo/scale", `{"process":"web","replicas":1}`, true); rec.Code != http.StatusOK {
		t.Errorf("scale to 1: %d %s", rec.Code, rec.Body.String())
	}

	// Delete: mounted volumes need force.
	vol := &shpyrdv1.Volume{}
	_ = cr.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "data"}, vol)
	vol.Status.MountedBy = []string{"demo/web"}
	if err := cr.Status().Update(context.Background(), vol); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, s, "DELETE", "/api/projects/demo/volumes/data", "", true); rec.Code != http.StatusConflict {
		t.Errorf("delete mounted: %d", rec.Code)
	}
	if rec := do(t, s, "DELETE", "/api/projects/demo/volumes/data?force=true", "", true); rec.Code != http.StatusNoContent {
		t.Errorf("force delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "DELETE", "/api/projects/demo/volumes/assets", "", true); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d", rec.Code)
	}
}

func TestProjectResourcesAndBoundVars(t *testing.T) {
	app := &shpyrdv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "app-demo"},
		Spec: shpyrdv1.AppSpec{
			Source:    &shpyrdv1.Source{Git: &shpyrdv1.GitSource{URL: "https://example.test/r"}},
			Bindings:  []shpyrdv1.Binding{{Kind: "Postgres", Name: "db"}},
			Processes: map[string]shpyrdv1.Process{"web": {Volumes: []shpyrdv1.VolumeMount{{Name: "data", Path: "/data"}}}},
		},
		Status: shpyrdv1.AppStatus{Phase: "Running", URL: "https://demo.example.test", Releases: []shpyrdv1.Release{{Number: 3, Image: "x"}}},
	}
	vol := &shpyrdv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app-demo"},
		Spec:       shpyrdv1.VolumeSpec{Size: resource.MustParse("5Gi")},
		Status:     shpyrdv1.VolumeStatus{Phase: "Bound", Capacity: "5Gi", MountedBy: []string{"demo/web"}},
	}
	bindings := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-bindings", Namespace: "app-demo", Annotations: map[string]string{
			shpyrdv1.AnnotationBindingProviders: `{"DATABASE_URL":"Postgres/db"}`,
		}},
		Data: map[string][]byte{"DATABASE_URL": []byte("postgres://secret")},
	}
	s, _ := newTestServer(t, nil, []client.Object{app, vol, bindings})

	rec := do(t, s, "GET", "/api/projects/demo/resources", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", rec.Code, rec.Body.String())
	}
	var res []ResourceView
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Kind != "App" || res[1].Kind != "Volume" {
		t.Fatalf("resources = %+v", res)
	}
	if res[0].Endpoint != "https://demo.example.test" || res[0].Details["release"] != "v3" || res[0].Details["build"] != "buildpacks" || res[0].Details["bindings"] != "Postgres/db" {
		t.Errorf("app view = %+v", res[0])
	}
	if !res[1].Data || res[1].Details["size"] != "5Gi" || res[1].Details["mode"] != "single-instance" || res[1].AttachedTo[0] != "demo/web" {
		t.Errorf("volume view = %+v", res[1])
	}

	rec = do(t, s, "GET", "/api/projects/demo/secrets", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("secrets: %d %s", rec.Code, rec.Body.String())
	}
	var cfg ConfigVarsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &cfg)
	if len(cfg.Bound) != 1 || cfg.Bound[0].Name != "DATABASE_URL" || cfg.Bound[0].Provider != "Postgres/db" {
		t.Errorf("bound vars = %+v", cfg.Bound)
	}
	if strings.Contains(rec.Body.String(), "postgres://secret") {
		t.Error("bound values must never be returned")
	}
}

func TestResourcesAndBindingsAPI(t *testing.T) {
	app := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "app-shop"}, Spec: shpyrdv1.AppSpec{Image: "x"}}
	s, cr := newTestServer(t, nil, []client.Object{app})
	s.opts.Extensions = all.All() // postgres and redis kinds become available

	// Create a Postgres and a Redis generically.
	rec := do(t, s, "POST", "/api/projects/shop/resources", `{"kind":"Postgres","name":"db","spec":{"version":"16","storage":"10Gi"}}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create postgres: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/shop/resources", `{"kind":"Redis","name":"cache","spec":{"persistent":false}}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("create redis: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/shop/resources", `{"kind":"Mongo","name":"x"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown kind: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects/shop/resources", `{"kind":"Postgres","name":"db"}`, true); rec.Code != http.StatusConflict {
		t.Errorf("duplicate: %d", rec.Code)
	}

	rec = do(t, s, "GET", "/api/projects/shop/resources", "", true)
	var res []ResourceView
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	kinds := map[string]ResourceView{}
	for _, r := range res {
		kinds[r.Kind] = r
	}
	if len(res) != 3 || kinds["Postgres"].Details["version"] != "16" || kinds["Postgres"].Details["storage"] != "10Gi" || !kinds["Postgres"].Bindable || kinds["Postgres"].Phase != "Pending" {
		t.Errorf("resources = %s", rec.Body.String())
	}

	// Attach: a binding with a release note; duplicates and unknown targets refused.
	rec = do(t, s, "POST", "/api/projects/shop/bindings", `{"kind":"Postgres","name":"db"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach: %d %s", rec.Code, rec.Body.String())
	}
	got := &shpyrdv1.App{}
	_ = cr.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "shop"}, got)
	if len(got.Spec.Bindings) != 1 || got.Spec.Bindings[0].Kind != "Postgres" || got.Annotations[shpyrdv1.AnnotationReleaseNote] != "Attach Postgres db" {
		t.Errorf("app after attach: %+v %v", got.Spec.Bindings, got.Annotations)
	}
	if rec := do(t, s, "POST", "/api/projects/shop/bindings", `{"kind":"Postgres","name":"db"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("double attach: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/shop/bindings", `{"kind":"Postgres","name":"nope"}`, true); rec.Code != http.StatusNotFound {
		t.Errorf("attach missing: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects/shop/bindings", `{"kind":"Redis","name":"cache","prefix":"bad prefix"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("bad prefix: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects/shop/bindings", `{"kind":"Redis","name":"cache","prefix":"queue"}`, true); rec.Code != http.StatusOK {
		t.Errorf("attach redis: %d %s", rec.Code, rec.Body.String())
	}
	_ = cr.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "shop"}, got)
	if len(got.Spec.Bindings) != 2 || got.Spec.Bindings[1].Prefix != "QUEUE" {
		t.Errorf("prefix upper-cased: %+v", got.Spec.Bindings)
	}

	// The resources list shows who attaches what; deletion is refused while attached.
	rec = do(t, s, "GET", "/api/projects/shop/resources", "", true)
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	for _, r := range res {
		if r.Kind == "Postgres" && (len(r.AttachedTo) != 1 || r.AttachedTo[0] != "shop") {
			t.Errorf("attachedTo = %v", r.AttachedTo)
		}
	}
	if rec := do(t, s, "DELETE", "/api/projects/shop/resources/Postgres/db", "", true); rec.Code != http.StatusConflict {
		t.Errorf("delete attached: %d %s", rec.Code, rec.Body.String())
	}

	// Detach, then delete.
	if rec := do(t, s, "DELETE", "/api/projects/shop/bindings/Postgres/db", "", true); rec.Code != http.StatusOK {
		t.Fatalf("detach: %d %s", rec.Code, rec.Body.String())
	}
	_ = cr.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "shop"}, got)
	if len(got.Spec.Bindings) != 1 || got.Spec.Bindings[0].Kind != "Redis" || got.Annotations[shpyrdv1.AnnotationReleaseNote] != "Detach Postgres db" {
		t.Errorf("app after detach: %+v %v", got.Spec.Bindings, got.Annotations)
	}
	if rec := do(t, s, "DELETE", "/api/projects/shop/bindings/Postgres/db", "", true); rec.Code != http.StatusBadRequest {
		t.Errorf("detach twice: %d", rec.Code)
	}
	if rec := do(t, s, "DELETE", "/api/projects/shop/resources/Postgres/db", "", true); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "DELETE", "/api/projects/shop/resources/Redis/cache?force=true", "", true); rec.Code != http.StatusNoContent {
		t.Errorf("force delete attached: %d %s", rec.Code, rec.Body.String())
	}
}

// RFC-0011: a project is created from a human name; the slug is derived or
// given, and paths name projects by slug only.
func TestProjectIdentity(t *testing.T) {
	s, k := newTestServer(t, nil, nil)
	rec := do(t, s, "POST", "/api/projects", `{"name":"  My Shop  "}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created AppSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Slug != "my-shop" || created.DisplayName != "My Shop" || created.Namespace != "app-my-shop" {
		t.Fatalf("created = %+v", created)
	}
	app := &shpyrdv1.App{}
	if err := k.Get(context.Background(), types.NamespacedName{Namespace: "app-my-shop", Name: "my-shop"}, app); err != nil {
		t.Fatal(err)
	}
	if app.Annotations[shpyrdv1.AnnotationDisplayName] != "My Shop" {
		t.Errorf("annotation = %v", app.Annotations)
	}

	// An explicit slug wins over the derived one.
	rec = do(t, s, "POST", "/api/projects", `{"name":"My Shop","slug":"shop-eu"}`, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"slug":"shop-eu"`) {
		t.Fatalf("explicit slug: %d %s", rec.Code, rec.Body.String())
	}
	// A name that already is a slug stores no annotation.
	rec = do(t, s, "POST", "/api/projects", `{"name":"plain"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plain: %d %s", rec.Code, rec.Body.String())
	}
	plain := &shpyrdv1.App{}
	_ = k.Get(context.Background(), types.NamespacedName{Namespace: "app-plain", Name: "plain"}, plain)
	if _, ok := plain.Annotations[shpyrdv1.AnnotationDisplayName]; ok {
		t.Errorf("plain slug must not carry a display-name annotation: %v", plain.Annotations)
	}

	// Detail and list carry both names; the list sorts by display name.
	rec = do(t, s, "GET", "/api/projects/my-shop", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"displayName":"My Shop"`) || !strings.Contains(rec.Body.String(), `"slug":"my-shop"`) {
		t.Errorf("detail: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "GET", "/api/projects", "", true)
	var list []AppSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 3 || list[0].Slug != "my-shop" || list[1].Slug != "shop-eu" || list[2].Slug != "plain" {
		t.Errorf("list order = %+v", list)
	}

	// Rename changes the display name only.
	rec = do(t, s, "PATCH", "/api/projects/my-shop", `{"name":"The Shop"}`, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"displayName":"The Shop"`) || !strings.Contains(rec.Body.String(), `"slug":"my-shop"`) {
		t.Errorf("rename: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "PATCH", "/api/projects/my-shop", `{"name":"  "}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("empty rename: %d", rec.Code)
	}

	// Paths use slugs: a namespace-looking or malformed one is a 404, not an
	// upstream error.
	for _, p := range []string{"/api/projects/app-my-shop", "/api/projects/My%20Shop", "/api/projects/-bad"} {
		if rec := do(t, s, "GET", p, "", true); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d want 404", p, rec.Code)
		}
	}
}

// RFC-0016: global config vars are set once, never read back, counted per
// project, and shown read-only to project members through their mirror.
func TestGlobals(t *testing.T) {
	optedOut := sampleApp("quiet", shpyrdv1.PhaseRunning)
	optedOut.Spec.Globals = &shpyrdv1.Globals{Disabled: true}
	s, k := newTestServer(t, nil, []client.Object{sampleApp("web1", shpyrdv1.PhaseRunning), optedOut})

	rec := do(t, s, "GET", "/api/globals", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"vars":[]`) || !strings.Contains(rec.Body.String(), `"projects":1`) {
		t.Fatalf("empty globals: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "PUT", "/api/globals", `{"set":{"OPENAI_API_KEY":"sk-secret"},"dotenv":"REGION=eu\n"}`, true)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "sk-secret") {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	var resp GlobalsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Vars) != 2 || resp.Vars[0].Name != "OPENAI_API_KEY" || resp.Vars[1].Name != "REGION" || resp.Vars[0].UpdatedAt == "" || resp.Projects != 1 {
		t.Errorf("resp = %+v", resp)
	}
	sec := &corev1.Secret{}
	if err := k.Get(context.Background(), types.NamespacedName{Namespace: "shpyrd-system", Name: shpyrdv1.GlobalEnvSecretName}, sec); err != nil || string(sec.Data["OPENAI_API_KEY"]) != "sk-secret" {
		t.Fatalf("secret: %v %v", err, sec.Data)
	}
	if rec := do(t, s, "PUT", "/api/globals", `{"unset":["REGION"]}`, true); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "REGION") {
		t.Errorf("unset: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "PUT", "/api/globals", `{"set":{"bad key":"x"}}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid key: %d", rec.Code)
	}
	if rec := do(t, s, "PUT", "/api/globals", `{}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("nothing to change: %d", rec.Code)
	}

	// A project's config view lists the globals it receives (from the
	// mirror the controller writes), without values.
	mirror := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: shpyrdv1.GlobalEnvSecretName, Namespace: "app-web1"}, Data: map[string][]byte{"OPENAI_API_KEY": []byte("sk-secret")}}
	if err := k.Create(context.Background(), mirror); err != nil {
		t.Fatal(err)
	}
	rec = do(t, s, "GET", "/api/projects/web1/secrets", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"global":[{"name":"OPENAI_API_KEY"}]`) || strings.Contains(rec.Body.String(), "sk-secret") {
		t.Errorf("project view: %d %s", rec.Code, rec.Body.String())
	}

	// Audit names the keys, never the values.
	evs, _ := s.kube.Kube.CoreV1().Events("shpyrd-system").List(context.Background(), metav1.ListOptions{})
	var set, unset int
	for _, ev := range evs.Items {
		switch ev.Annotations["shpyrd.io/action"] {
		case "globals.set":
			set++
			if strings.Contains(ev.Annotations["shpyrd.io/detail"], "sk-secret") {
				t.Error("audit leaked a value")
			}
		case "globals.unset":
			unset++
		}
	}
	if set != 1 || unset != 1 {
		t.Errorf("audit: set=%d unset=%d", set, unset)
	}
}

// RFC-0023: drains in two scopes; header values are written, never read.
func TestDrains(t *testing.T) {
	s, k := newTestServer(t, nil, []client.Object{sampleApp("shop", shpyrdv1.PhaseRunning)})

	// Project drain with headers; name derived from the host.
	rec := do(t, s, "POST", "/api/projects/shop/drains", `{"url":"https://in.logs.example.com/ingest","headers":{"Authorization":"Bearer s3cret"},"processes":["web"]}`, true)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var v DrainView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Name != "in-logs-example-com" || v.Format != "json" || v.Cluster || len(v.Headers) != 1 || v.Headers[0] != "Authorization" || v.Phase != "Pending" {
		t.Errorf("view = %+v", v)
	}
	sec := &corev1.Secret{}
	if err := k.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "drain-in-logs-example-com-headers"}, sec); err != nil || string(sec.Data["Authorization"]) != "Bearer s3cret" {
		t.Fatalf("headers secret: %v %v", err, sec.Data)
	}
	// Listing never returns values.
	rec = do(t, s, "GET", "/api/projects/shop/drains", "", true)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "s3cret") || !strings.Contains(rec.Body.String(), `"headers":["Authorization"]`) {
		t.Errorf("list: %d %s", rec.Code, rec.Body.String())
	}
	// Bad URLs and formats are refused.
	if rec := do(t, s, "POST", "/api/projects/shop/drains", `{"url":"ftp://x/"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("ftp: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects/shop/drains", `{"url":"syslog://logs.example.com","name":"nop"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("syslog without port: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/shop/drains", `{"url":"https://in.logs.example.com/ingest"}`, true); rec.Code != http.StatusConflict {
		t.Errorf("duplicate: %d", rec.Code)
	}

	// Cluster drain: syslog, format derived from the scheme, lives in the system namespace.
	rec = do(t, s, "POST", "/api/drains", `{"name":"siem","url":"syslog+tls://siem.example.com:6514"}`, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"cluster":true`) || !strings.Contains(rec.Body.String(), `"format":"syslog"`) {
		t.Fatalf("cluster drain: %d %s", rec.Code, rec.Body.String())
	}
	d := &shpyrdv1.LogDrain{}
	if err := k.Get(context.Background(), types.NamespacedName{Namespace: "shpyrd-system", Name: "siem"}, d); err != nil {
		t.Fatal(err)
	}
	rec = do(t, s, "GET", "/api/drains", "", true)
	if !strings.Contains(rec.Body.String(), `"name":"siem"`) || strings.Contains(rec.Body.String(), "in-logs-example-com") {
		t.Errorf("cluster list must not include project drains: %s", rec.Body.String())
	}

	// Delete removes the drain and its Secret.
	if rec := do(t, s, "DELETE", "/api/projects/shop/drains/in-logs-example-com", "", true); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if err := k.Get(context.Background(), types.NamespacedName{Namespace: "app-shop", Name: "drain-in-logs-example-com-headers"}, sec); err == nil {
		t.Error("headers secret should be gone")
	}
	if rec := do(t, s, "DELETE", "/api/projects/shop/drains/in-logs-example-com", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("delete twice: %d", rec.Code)
	}

	// Audit names the drain, not its headers.
	evs, _ := s.kube.Kube.CoreV1().Events("shpyrd-system").List(context.Background(), metav1.ListOptions{})
	found := false
	for _, ev := range evs.Items {
		if ev.Annotations["shpyrd.io/action"] == "drain.add" && strings.Contains(ev.Annotations["shpyrd.io/target"], "siem") {
			found = true
		}
		if strings.Contains(ev.Annotations["shpyrd.io/detail"], "s3cret") {
			t.Error("audit leaked a header value")
		}
	}
	if !found {
		t.Error("cluster drain.add not audited")
	}
}

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// Chart is one panel of the app metrics view.
type Chart struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Unit   string   `json:"unit"`   // rps, ms, cores, bytes, count, bytes/s
	Kind   string   `json:"kind"`   // line or stacked
	Series []Series `json:"series"` // one or more named series
	Error  string   `json:"error,omitempty"`
}

// Series is a named time series.
type Series struct {
	Name   string  `json:"name"`
	Points []Point `json:"points"`
}

// ReleaseMarker lets the UI draw deploy lines on the charts.
type ReleaseMarker struct {
	Number int     `json:"number"`
	Time   float64 `json:"time"`
	Label  string  `json:"label"`
}

// MetricsResponse holds the charts for an app.
type MetricsResponse struct {
	Range    string          `json:"range"`
	Step     int             `json:"step"`
	Charts   []Chart         `json:"charts"`
	Releases []ReleaseMarker `json:"releases"`
}

var rangeOptions = map[string]time.Duration{"1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

// chartQuery describes how to build one chart. Multi-series queries return
// one series per label value (labelKey); fixed queries produce a series
// each with a given name.
type chartQuery struct {
	Chart
	// labelKey: name series after this label of a single query (Query).
	Query    string
	LabelKey string
	// nameMap post-processes label values (e.g. strip a prefix).
	nameMap func(string) string
	// fixed: several queries, each one series.
	Fixed map[string]string
	// fallback runs when Query returns nothing (e.g. kube-state-metrics
	// label allowlist not applied yet, or pods without limits).
	Fallback     string
	FallbackName string
	FallbackUnit string
}

func (s *Server) appMetrics(c *gin.Context) {
	if s.prom == nil {
		abort(c, http.StatusNotImplemented, errors.New("metrics are not configured"))
		return
	}
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	rng := c.DefaultQuery("range", "1h")
	dur, ok := rangeOptions[rng]
	if !ok {
		abort(c, http.StatusBadRequest, errors.New("range must be one of 1h, 6h, 24h, 7d"))
		return
	}
	end := time.Now().Truncate(time.Minute)
	start := end.Add(-dur)
	step := dur / 60
	if step < time.Minute {
		step = time.Minute
	}

	queries := chartQueries(app)
	resp := MetricsResponse{Range: rng, Step: int(step.Seconds()), Charts: make([]Chart, len(queries)), Releases: []ReleaseMarker{}}
	for _, r := range app.Status.Releases {
		if r.CreatedAt.Time.After(start) {
			resp.Releases = append(resp.Releases, ReleaseMarker{Number: r.Number, Time: float64(r.CreatedAt.Unix()), Label: "v" + fmt.Sprint(r.Number)})
		}
	}

	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q chartQuery) {
			defer wg.Done()
			resp.Charts[i] = s.runChart(c.Request.Context(), q, start, end, step)
		}(i, q)
	}
	wg.Wait()
	c.JSON(http.StatusOK, resp)
}

func (s *Server) runChart(ctx context.Context, q chartQuery, start, end time.Time, step time.Duration) Chart {
	ch := q.Chart
	ch.Series = []Series{}
	if len(q.Fixed) > 0 {
		names := make([]string, 0, len(q.Fixed))
		for n := range q.Fixed {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			pts, err := s.prom.QueryRange(ctx, q.Fixed[n], start, end, step)
			if err != nil {
				ch.Error = err.Error()
				return ch
			}
			ch.Series = append(ch.Series, Series{Name: n, Points: pts})
		}
		return ch
	}
	raw, err := s.prom.QueryRangeSeries(ctx, q.Query, start, end, step)
	if err != nil {
		ch.Error = err.Error()
		return ch
	}
	if len(raw) == 0 && q.Fallback != "" {
		pts, err := s.prom.QueryRange(ctx, q.Fallback, start, end, step)
		if err != nil {
			ch.Error = err.Error()
			return ch
		}
		if len(pts) > 0 {
			ch.Series = append(ch.Series, Series{Name: q.FallbackName, Points: pts})
			if q.FallbackUnit != "" {
				ch.Unit = q.FallbackUnit
			}
		}
		return ch
	}
	for _, r := range raw {
		name := r.Labels[q.LabelKey]
		if q.nameMap != nil {
			name = q.nameMap(name)
		}
		if name == "" {
			name = "all"
		}
		ch.Series = append(ch.Series, Series{Name: name, Points: r.Points})
	}
	sort.Slice(ch.Series, func(i, j int) bool { return ch.Series[i].Name < ch.Series[j].Name })
	return ch
}

// chartQueries defines the app dashboard, modelled on what Heroku, Fly,
// Render and Railway show: throughput by status class, response time
// percentiles, instance count, CPU and memory per process type, network.
func chartQueries(app *shpyrdv1.App) []chartQuery {
	hosts := hostRegex(app)
	ns := app.Namespace
	name := app.Name
	podLabels := fmt.Sprintf(`kube_pod_labels{namespace="%s",label_shpyrd_io_app="%s"}`, ns, name)
	containers := fmt.Sprintf(`namespace="%s",container!="",container!="POD"`, ns)
	stripApp := func(s string) string { return strings.TrimPrefix(s, name+"-") }

	return []chartQuery{
		{
			Chart:    Chart{ID: "throughput", Title: "Throughput", Unit: "rps", Kind: "stacked"},
			Query:    fmt.Sprintf(`sum by (class) (label_replace(rate(nginx_ingress_controller_requests{host=~"%s"}[2m]), "class", "${1}xx", "status", "(.).."))`, hosts),
			LabelKey: "class",
		},
		{
			Chart: Chart{ID: "latency", Title: "Response time", Unit: "ms", Kind: "line"},
			Fixed: map[string]string{
				"p50": fmt.Sprintf(`histogram_quantile(0.50, sum by (le) (rate(nginx_ingress_controller_request_duration_seconds_bucket{host=~"%s"}[5m]))) * 1000`, hosts),
				"p95": fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(nginx_ingress_controller_request_duration_seconds_bucket{host=~"%s"}[5m]))) * 1000`, hosts),
				"p99": fmt.Sprintf(`histogram_quantile(0.99, sum by (le) (rate(nginx_ingress_controller_request_duration_seconds_bucket{host=~"%s"}[5m]))) * 1000`, hosts),
			},
		},
		{
			Chart:    Chart{ID: "instances", Title: "Instances", Unit: "count", Kind: "step"},
			Query:    fmt.Sprintf(`sum by (deployment) (kube_deployment_status_replicas_available{namespace="%s"})`, ns),
			LabelKey: "deployment",
			nameMap:  stripApp,
		},
		{
			// Usage as a percentage of the process allocation (the CPU
			// request, i.e. the instance size), averaged over its instances.
			// Shared sizes may burst above 100%. Falls back to raw cores when
			// no requests exist.
			Chart: Chart{ID: "cpu", Title: "CPU", Unit: "%", Kind: "line"},
			Query: fmt.Sprintf(`100 * sum by (label_shpyrd_io_process) (rate(container_cpu_usage_seconds_total{%s}[2m]) * on (namespace, pod) group_left (label_shpyrd_io_process) %s)`+
				` / sum by (label_shpyrd_io_process) (kube_pod_container_resource_requests{%s,resource="cpu"} * on (namespace, pod) group_left (label_shpyrd_io_process) %s)`,
				containers, podLabels, containers, podLabels),
			LabelKey:     "label_shpyrd_io_process",
			Fallback:     fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{%s}[2m]))`, containers),
			FallbackName: "all",
			FallbackUnit: "cores",
		},
		{
			Chart: Chart{ID: "memory", Title: "Memory", Unit: "%", Kind: "line"},
			Query: fmt.Sprintf(`100 * sum by (label_shpyrd_io_process) (container_memory_working_set_bytes{%s} * on (namespace, pod) group_left (label_shpyrd_io_process) %s)`+
				` / sum by (label_shpyrd_io_process) (kube_pod_container_resource_requests{%s,resource="memory"} * on (namespace, pod) group_left (label_shpyrd_io_process) %s)`,
				containers, podLabels, containers, podLabels),
			LabelKey:     "label_shpyrd_io_process",
			Fallback:     fmt.Sprintf(`sum(container_memory_working_set_bytes{%s})`, containers),
			FallbackName: "all",
			FallbackUnit: "bytes",
		},
		{
			Chart: Chart{ID: "network", Title: "Network", Unit: "bytes/s", Kind: "line"},
			Fixed: map[string]string{
				"in":  fmt.Sprintf(`sum(rate(container_network_receive_bytes_total{namespace="%s"}[2m]))`, ns),
				"out": fmt.Sprintf(`sum(rate(container_network_transmit_bytes_total{namespace="%s"}[2m]))`, ns),
			},
		},
	}
}

// hostRegex builds a PromQL regex matching the app's ingress hosts.
func hostRegex(app *shpyrdv1.App) string {
	hosts := app.Spec.Domains
	if len(hosts) == 0 && app.Status.URL != "" {
		h := strings.TrimPrefix(app.Status.URL, "https://")
		if i := strings.IndexByte(h, ':'); i >= 0 {
			h = h[:i]
		}
		hosts = []string{h}
	}
	quoted := make([]string, 0, len(hosts))
	for _, h := range hosts {
		quoted = append(quoted, regexp.QuoteMeta(h))
	}
	if len(quoted) == 0 {
		return "^$"
	}
	// PromQL regexes are RE2 in a double-quoted string; escape backslashes.
	return strings.ReplaceAll(strings.Join(quoted, "|"), `\`, `\\`)
}

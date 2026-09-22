package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// NodeUsage is the current utilisation of one node.
type NodeUsage struct {
	Name string `json:"name"`
	// Used percentages come from node-exporter (what the machine is doing).
	CPUUsedPct    float64 `json:"cpuUsedPct"`
	MemoryUsedPct float64 `json:"memoryUsedPct"`
	// Requested percentages come from kube-state-metrics (what has been
	// reserved by pod requests; the scheduler's view).
	CPURequestedPct    float64 `json:"cpuRequestedPct"`
	MemoryRequestedPct float64 `json:"memoryRequestedPct"`
	CPUCores           float64 `json:"cpuCores"`
	MemoryBytes        float64 `json:"memoryBytes"`
	Pods               float64 `json:"pods"`
	PodCapacity        float64 `json:"podCapacity"`
}

// ClusterMetrics feeds the cluster page's capacity section.
type ClusterMetrics struct {
	Range  string      `json:"range"`
	Nodes  []NodeUsage `json:"nodes"`
	Total  NodeUsage   `json:"total"`
	Charts []Chart     `json:"charts"`
}

// Instant queries per node, keyed by the label carrying the node name.
var nodeQueries = map[string]struct{ query, label string }{
	"cpuUsed":  {`100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[2m]))) * on (instance) group_left (nodename) node_uname_info`, "nodename"},
	"memUsed":  {`100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes) * on (instance) group_left (nodename) node_uname_info`, "nodename"},
	"cpuReq":   {`100 * sum by (node) (kube_pod_container_resource_requests{resource="cpu"}) / on (node) kube_node_status_allocatable{resource="cpu"}`, "node"},
	"memReq":   {`100 * sum by (node) (kube_pod_container_resource_requests{resource="memory"}) / on (node) kube_node_status_allocatable{resource="memory"}`, "node"},
	"cpuCores": {`kube_node_status_allocatable{resource="cpu"}`, "node"},
	"memBytes": {`kube_node_status_allocatable{resource="memory"}`, "node"},
	"pods":     {`count by (node) (kube_pod_info)`, "node"},
	"podCap":   {`kube_node_status_allocatable{resource="pods"}`, "node"},
}

func (s *Server) clusterMetrics(c *gin.Context) {
	if s.prom == nil {
		abort(c, http.StatusNotImplemented, errors.New("metrics are not configured"))
		return
	}
	rng := c.DefaultQuery("range", "1h")
	dur, ok := rangeOptions[rng]
	if !ok {
		abort(c, http.StatusBadRequest, errors.New("range must be one of 1h, 6h, 24h, 7d"))
		return
	}
	ctx := c.Request.Context()
	nodes := map[string]*NodeUsage{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	get := func(name string) *NodeUsage {
		n, ok := nodes[name]
		if !ok {
			n = &NodeUsage{Name: name}
			nodes[name] = n
		}
		return n
	}
	for key, q := range nodeQueries {
		wg.Add(1)
		go func(key string, query, label string) {
			defer wg.Done()
			samples, err := s.prom.Query(ctx, query)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, smp := range samples {
				name := smp.Labels[label]
				if name == "" {
					continue
				}
				n := get(name)
				switch key {
				case "cpuUsed":
					n.CPUUsedPct = smp.Value
				case "memUsed":
					n.MemoryUsedPct = smp.Value
				case "cpuReq":
					n.CPURequestedPct = smp.Value
				case "memReq":
					n.MemoryRequestedPct = smp.Value
				case "cpuCores":
					n.CPUCores = smp.Value
				case "memBytes":
					n.MemoryBytes = smp.Value
				case "pods":
					n.Pods = smp.Value
				case "podCap":
					n.PodCapacity = smp.Value
				}
			}
		}(key, q.query, q.label)
	}
	wg.Wait()
	if firstErr != nil && len(nodes) == 0 {
		abort(c, http.StatusBadGateway, firstErr)
		return
	}

	out := ClusterMetrics{Range: rng, Nodes: []NodeUsage{}, Charts: []Chart{}}
	var total NodeUsage
	total.Name = "cluster"
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, *n)
		// Weighted by capacity so big nodes count more.
		total.CPUUsedPct += n.CPUUsedPct * n.CPUCores
		total.CPURequestedPct += n.CPURequestedPct * n.CPUCores
		total.MemoryUsedPct += n.MemoryUsedPct * n.MemoryBytes
		total.MemoryRequestedPct += n.MemoryRequestedPct * n.MemoryBytes
		total.CPUCores += n.CPUCores
		total.MemoryBytes += n.MemoryBytes
		total.Pods += n.Pods
		total.PodCapacity += n.PodCapacity
	}
	if total.CPUCores > 0 {
		total.CPUUsedPct /= total.CPUCores
		total.CPURequestedPct /= total.CPUCores
	}
	if total.MemoryBytes > 0 {
		total.MemoryUsedPct /= total.MemoryBytes
		total.MemoryRequestedPct /= total.MemoryBytes
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].Name < out.Nodes[j].Name })
	out.Total = total

	// Utilisation over time per node.
	end := time.Now().Truncate(time.Minute)
	start := end.Add(-dur)
	step := dur / 60
	if step < time.Minute {
		step = time.Minute
	}
	charts := []chartQuery{
		{Chart: Chart{ID: "cpu", Title: "CPU used", Unit: "%", Kind: "line"}, Query: nodeQueries["cpuUsed"].query, LabelKey: "nodename"},
		{Chart: Chart{ID: "memory", Title: "Memory used", Unit: "%", Kind: "line"}, Query: nodeQueries["memUsed"].query, LabelKey: "nodename"},
	}
	out.Charts = make([]Chart, len(charts))
	var cwg sync.WaitGroup
	for i, q := range charts {
		cwg.Add(1)
		go func(i int, q chartQuery) {
			defer cwg.Done()
			out.Charts[i] = s.runChart(ctx, q, start, end, step)
		}(i, q)
	}
	cwg.Wait()
	c.JSON(http.StatusOK, out)
}

// Sample is one instant query result.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Query evaluates an instant query.
func (p *PromClient) Query(ctx context.Context, query string) ([]Sample, error) {
	return p.instant(ctx, query)
}

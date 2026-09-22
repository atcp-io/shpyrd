package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PromClient is a minimal Prometheus HTTP API client.
type PromClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewPromClient returns a client for baseURL (e.g. http://prometheus:9090).
func NewPromClient(baseURL string) *PromClient {
	return &PromClient{BaseURL: baseURL, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// Point is one sample: unix seconds and value.
type Point [2]float64

// RawSeries is one result series of a range query.
type RawSeries struct {
	Labels map[string]string
	Points []Point
}

// QueryRangeSeries evaluates query over [start, end] at step and returns
// every result series. Prometheus NaN/Inf values are dropped.
func (p *PromClient) QueryRangeSeries(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]RawSeries, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.FormatInt(int64(step.Seconds()), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := p.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	defer res.Body.Close()
	var body struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]interface{}  `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("prometheus: decode: %w", err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s: %s", body.ErrorType, body.Error)
	}
	out := make([]RawSeries, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		rs := RawSeries{Labels: r.Metric}
		for _, v := range r.Values {
			ts, ok := v[0].(float64)
			if !ok {
				continue
			}
			s, _ := v[1].(string)
			f, err := strconv.ParseFloat(s, 64)
			if err != nil || f != f || f > 1e300 || f < -1e300 { // NaN / Inf
				continue
			}
			rs.Points = append(rs.Points, Point{ts, f})
		}
		if len(rs.Points) > 0 {
			out = append(out, rs)
		}
	}
	return out, nil
}

// QueryRange evaluates query and sums the result series per timestamp
// into one series (for queries written to return a single series).
func (p *PromClient) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Point, error) {
	series, err := p.QueryRangeSeries(ctx, query, start, end, step)
	if err != nil {
		return nil, err
	}
	acc := map[float64]float64{}
	var order []float64
	for _, s := range series {
		for _, pt := range s.Points {
			if _, seen := acc[pt[0]]; !seen {
				order = append(order, pt[0])
			}
			acc[pt[0]] += pt[1]
		}
	}
	out := make([]Point, 0, len(order))
	for _, ts := range order {
		out = append(out, Point{ts, acc[ts]})
	}
	return out, nil
}

// instant runs /api/v1/query and returns the vector result.
func (p *PromClient) instant(ctx context.Context, query string) ([]Sample, error) {
	q := url.Values{}
	q.Set("query", query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/api/v1/query?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := p.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	defer res.Body.Close()
	var body struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]interface{}    `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("prometheus: decode: %w", err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s: %s", body.ErrorType, body.Error)
	}
	out := make([]Sample, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		s, _ := r.Value[1].(string)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f != f || f > 1e300 || f < -1e300 {
			continue
		}
		out = append(out, Sample{Labels: r.Metric, Value: f})
	}
	return out, nil
}

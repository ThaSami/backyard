package server

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics are the Prometheus metrics of SPEC §15, in a private registry.
type metrics struct {
	reg     *prometheus.Registry
	handler http.Handler

	httpRequests     *prometheus.CounterVec
	httpDuration     *prometheus.HistogramVec
	deploys          *prometheus.CounterVec
	dbOps            *prometheus.CounterVec
	aiRequests       *prometheus.CounterVec
	aiTokens         *prometheus.CounterVec
	wsMessages       *prometheus.CounterVec
	warehouseQueries *prometheus.CounterVec
	rateLimited      *prometheus.CounterVec
}

// newMetrics builds the registry. wsConns (optional) reports open websocket
// connections.
func newMetrics(version string, wsConns func() float64) *metrics {
	const ns = "backyard"
	reg := prometheus.NewRegistry()
	m := &metrics{
		reg: reg,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "http_requests_total", Help: "HTTP requests by area and status code.",
		}, []string{"area", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "http_request_duration_seconds", Help: "HTTP request latency by area.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"area"}),
		deploys: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "deploys_total", Help: "Deploy commits (api) and refreshes after direct syncs (sync) by result.",
		}, []string{"method", "result"}),
		dbOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "db_ops_total", Help: "Document store operations by operation and result.",
		}, []string{"op", "result"}),
		aiRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "ai_requests_total", Help: "AI requests by model alias and result.",
		}, []string{"model", "result"}),
		aiTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "ai_tokens_total", Help: "AI tokens by model alias and direction (input|output).",
		}, []string{"model", "direction"}),
		wsMessages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "ws_messages_total", Help: "Websocket frames by direction (in|out).",
		}, []string{"dir"}),
		warehouseQueries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "warehouse_queries_total", Help: "Warehouse queries by source and result.",
		}, []string{"source", "result"}),
		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "rate_limited_total", Help: "Requests rejected by rate limits, by category.",
		}, []string{"category"}),
	}
	if wsConns == nil {
		wsConns = func() float64 { return 0 }
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.httpRequests, m.httpDuration, m.deploys, m.dbOps, m.aiRequests, m.aiTokens,
		m.wsMessages, m.warehouseQueries, m.rateLimited,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: ns, Name: "ws_connections", Help: "Open websocket connections on this instance.",
		}, wsConns),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: ns, Name: "build_info", Help: "Build information.",
			ConstLabels: prometheus.Labels{"version": version},
		}, func() float64 { return 1 }),
	)
	// Pre-create the series of the common labels so dashboards see zeros.
	for _, dir := range []string{"in", "out"} {
		m.wsMessages.WithLabelValues(dir)
	}
	m.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
	return m
}

// Package metrics defines Gordon's Prometheus metrics. Every label takes
// its value from a fixed set (sources, families, outcomes, action kinds,
// providers, queue states); a value from a payload outside the set is
// recorded as "other", so no delivery can grow the series count.
package metrics

import (
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Other replaces a label value outside its fixed set.
const Other = "other"

// None is the family label of a decision taken before any family.
const None = "none"

const namespace = "gordon"

// Buckets of the latency histograms, in seconds: from a fast API call to
// an item waiting out a dependency outage.
var (
	callBuckets  = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	queueBuckets = []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900, 3600, 4 * 3600, 24 * 3600}
)

// Metrics holds every collector, registered on Registry.
type Metrics struct {
	Registry *prometheus.Registry

	Intake          *prometheus.CounterVec
	Decisions       *prometheus.CounterVec
	DecisionLatency *prometheus.HistogramVec
	InboxRetries    *prometheus.CounterVec
	InboxFailures   *prometheus.CounterVec
	Deferred        *prometheus.CounterVec
	QueueDepth      *prometheus.GaugeVec
	QueueOldest     *prometheus.GaugeVec

	ActionResults      *prometheus.CounterVec
	ActionDuration     *prometheus.HistogramVec
	ActionQueueLatency *prometheus.HistogramVec

	Upstream         *prometheus.CounterVec
	UpstreamDuration *prometheus.HistogramVec

	Kelos             *prometheus.CounterVec
	KelosDuration     *prometheus.HistogramVec
	Submissions       *prometheus.CounterVec
	ShadowComparisons *prometheus.CounterVec
	RunObservations   *prometheus.CounterVec

	Reconciles           *prometheus.CounterVec
	ReconcileDuration    prometheus.Histogram
	ReconcileLastSuccess prometheus.Gauge
	LeasesExpired        *prometheus.CounterVec
	BodiesPruned         prometheus.Counter
}

// New registers Gordon's metrics, with the Go and process collectors, on
// a new registry.
func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		Intake: counter("intake_requests_total", "Webhook requests answered by the receiver.",
			"source", "result", "reason"),
		Decisions: counter("decisions_total", "Routing decisions recorded, per family.",
			"source", "family", "outcome", "executed"),
		DecisionLatency: histogram("decision_latency_seconds",
			"Time from a delivery's receipt to its recorded decision.", queueBuckets, "source"),
		InboxRetries: counter("inbox_retries_total", "Inbox items returned to the queue for a retry.",
			"source", "reason"),
		InboxFailures: counter("inbox_failures_total", "Inbox items dead-lettered after their last attempt.",
			"source"),
		Deferred: counter("deferred_total", "Claims returned unprocessed because a concurrency limit was full.",
			"queue", "limit"),
		QueueDepth: gauge("queue_depth", "Rows per queue and state (done rows excluded).", "queue", "state"),
		QueueOldest: gauge("queue_oldest_age_seconds", "Age of the oldest row per queue and state.",
			"queue", "state"),
		ActionResults: counter("action_attempts_total", "Outbox action attempts by result.", "kind", "result"),
		ActionDuration: histogram("action_duration_seconds", "Duration of one action attempt.",
			callBuckets, "kind"),
		ActionQueueLatency: histogram("action_latency_seconds",
			"Time from an action's queueing to its successful completion (done or recovered).", queueBuckets, "kind"),
		Upstream: counter("upstream_requests_total", "Read requests to GitHub and Vikunja by result.",
			"provider", "result"),
		UpstreamDuration: histogram("upstream_request_duration_seconds", "Duration of one upstream request.",
			callBuckets, "provider"),
		Kelos: counter("kelos_requests_total", "Kelos Task API requests by operation and result.", "op", "result"),
		KelosDuration: histogram("kelos_request_duration_seconds", "Duration of one Kelos Task API request.",
			callBuckets, "op"),
		Submissions: counter("kelos_submissions_total", "Task submissions by result.", "result"),
		ShadowComparisons: counter("shadow_comparisons_total",
			"Shadow Tasks compared with the legacy Task of the same name, by family and result.", "family", "result"),
		RunObservations: counter("run_observations_total", "Run states recorded from Kelos, by state.", "state"),
		Reconciles:      counter("reconcile_runs_total", "Periodic reconciliation runs by result.", "result"),
		ReconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: namespace,
			Name: "reconcile_duration_seconds", Help: "Duration of one reconciliation run.", Buckets: callBuckets}),
		ReconcileLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace,
			Name: "reconcile_last_success_timestamp_seconds", Help: "Unix time of the last successful reconciliation."}),
		LeasesExpired: counter("leases_expired_total", "Leases of stalled or crashed workers released.", "queue"),
		BodiesPruned: prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace,
			Name: "bodies_pruned_total", Help: "Stored delivery bodies and built Task prompts dropped after their retention."}),
	}
	m.Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.Intake, m.Decisions, m.DecisionLatency, m.InboxRetries, m.InboxFailures, m.Deferred,
		m.QueueDepth, m.QueueOldest, m.ActionResults, m.ActionDuration, m.ActionQueueLatency,
		m.Upstream, m.UpstreamDuration, m.Kelos, m.KelosDuration, m.Submissions, m.ShadowComparisons,
		m.RunObservations, m.Reconciles, m.ReconcileDuration, m.ReconcileLastSuccess,
		m.LeasesExpired, m.BodiesPruned)
	return m
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
}

func gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
}

func histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help,
		Buckets: buckets}, labels)
}

// Bound returns value when it is one of allowed, else Other.
func Bound(value string, allowed []string) string {
	if slices.Contains(allowed, value) {
		return value
	}
	return Other
}

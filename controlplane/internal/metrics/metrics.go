// Package metrics defines the Prometheus instruments exported by the control plane.
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	// HTTPRequestsTotal counts HTTP requests by method and status code.
	HTTPRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "proteos_controlplane_http_requests_total",
		Help: "Total HTTP requests processed by the control plane.",
	}, []string{"method", "code"})

	// HTTPRequestDuration measures HTTP request latency.
	HTTPRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "proteos_controlplane_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})

	// MachinesByState tracks the instantaneous count of machines per lifecycle state.
	MachinesByState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proteos_controlplane_machines_by_state",
		Help: "Current number of machines per lifecycle state.",
	}, []string{"state"})
)

// Register registers all control-plane metrics with the default Prometheus registry.
func Register() {
	prometheus.MustRegister(
		HTTPRequestsTotal,
		HTTPRequestDuration,
		MachinesByState,
	)
}

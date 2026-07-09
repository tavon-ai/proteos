// Package metrics defines the Prometheus instruments exported by the node-agent.
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	// VMOperationsTotal counts VM lifecycle operations by op name and result.
	VMOperationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "proteos_nodeagent_vm_operations_total",
		Help: "Total VM lifecycle operations completed.",
	}, []string{"op", "result"})

	// VMOperationDuration measures the wall-clock time of VM lifecycle operations.
	VMOperationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "proteos_nodeagent_vm_operation_duration_seconds",
		Help:    "Duration of VM lifecycle operations in seconds.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300},
	}, []string{"op"})

	// VMsByState tracks the instantaneous count of VMs per driver state.
	VMsByState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proteos_nodeagent_vms_by_state",
		Help: "Current number of VMs per driver state.",
	}, []string{"state"})

	// FCLogLinesTotal counts Firecracker log lines forwarded, by level.
	FCLogLinesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "proteos_nodeagent_fc_log_lines_total",
		Help: "Firecracker structured log lines captured, by log level.",
	}, []string{"level"})
)

// Register registers all node-agent metrics with the default Prometheus registry.
func Register() {
	prometheus.MustRegister(
		VMOperationsTotal,
		VMOperationDuration,
		VMsByState,
		FCLogLinesTotal,
	)
}

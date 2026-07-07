// Package metrics defines the Prometheus instruments exported by the guest agent.
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	// PTYSessionsActive is the number of live PTY sessions at any moment.
	PTYSessionsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "proteos_guestagent_pty_sessions_active",
		Help: "Number of live PTY sessions managed by the guest agent.",
	})

	// PTYSessionsTotal counts every PTY session that has been created.
	PTYSessionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "proteos_guestagent_pty_sessions_total",
		Help: "Total PTY sessions created since the guest agent started.",
	})
)

// Register registers all guest-agent metrics with the default Prometheus registry.
func Register() {
	prometheus.MustRegister(
		PTYSessionsActive,
		PTYSessionsTotal,
	)
}

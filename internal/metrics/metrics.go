// Package metrics exposes the Prometheus metrics from design #1, section 3.4.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
)

var (
	lastRun = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "schedule_pitcher_check_last_run_timestamp_seconds",
		Help: "Unix time of the last run of a check.",
	}, []string{"check"})
	checkStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "schedule_pitcher_check_status",
		Help: "Band of a check: 0 ok, 1 warning, 2 error, 3 critical, -1 unknown.",
	}, []string{"check"})
	checkFailing = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "schedule_pitcher_check_failing",
		Help: "1 while a check cannot complete (could not check).",
	}, []string{"check"})
	expiry = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "schedule_pitcher_check_expiry_seconds",
		Help: "Seconds until the watched thing expires (negative when expired).",
	}, []string{"check"})
	heartbeat = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "schedule_pitcher_heartbeat_timestamp_seconds",
		Help: "Unix time of the last heartbeat message; alert when it stops moving.",
	})
	sourceLastReport = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "schedule_pitcher_source_last_report_timestamp_seconds",
		Help: "Unix time of the last findings report of a source.",
	}, []string{"source"})
	pitches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "schedule_pitcher_pitch_total",
		Help: "Pitched messages by result (success, error).",
	}, []string{"result"})
)

// Registry holds this service's metrics plus the Go and process collectors.
var Registry = prometheus.NewRegistry()

func init() {
	Registry.MustRegister(lastRun, checkStatus, checkFailing, expiry, pitches, heartbeat, sourceLastReport,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// ObserveState records the state of a check after a run.
func ObserveState(s state.State) {
	lastRun.WithLabelValues(s.CheckID).Set(float64(s.LastRun.Unix()))
	checkStatus.WithLabelValues(s.CheckID).Set(float64(s.Band))
	failing := 0.0
	if s.Failing {
		failing = 1
	}
	checkFailing.WithLabelValues(s.CheckID).Set(failing)
	if s.Expiry.IsZero() {
		expiry.DeleteLabelValues(s.CheckID)
	} else {
		expiry.WithLabelValues(s.CheckID).Set(s.Expiry.Sub(s.LastRun).Seconds())
	}
}

// Heartbeat records a heartbeat message.
func Heartbeat(at time.Time) { heartbeat.Set(float64(at.Unix())) }

// SourceReported records the last report of a findings source.
func SourceReported(source string, at time.Time) {
	sourceLastReport.WithLabelValues(source).Set(float64(at.Unix()))
}

// Pitched counts a pitch attempt.
func Pitched(err error) {
	if err != nil {
		pitches.WithLabelValues("error").Inc()
		return
	}
	pitches.WithLabelValues("success").Inc()
}

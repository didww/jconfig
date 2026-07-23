// Package metrics defines the Prometheus surface jconfig exposes.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const namespace = "jconfig"

// Result label values.
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
)

// Git operation label values.
const (
	OpCommit = "commit"
	OpPush   = "push"
	OpStatus = "status"
	OpOpen   = "open"
	OpClone  = "clone"
)

// Metrics holds every collector jconfig exports.
type Metrics struct {
	Registry *prometheus.Registry

	BuildInfo  *prometheus.GaugeVec
	ConfigLoad *prometheus.GaugeVec

	DeviceInfo          *prometheus.GaugeVec
	BackupSuccess       *prometheus.GaugeVec
	BackupLastAttempt   *prometheus.GaugeVec
	BackupLastSuccess   *prometheus.GaugeVec
	BackupLastDuration  *prometheus.GaugeVec
	BackupDuration      *prometheus.HistogramVec
	BackupTotal         *prometheus.CounterVec
	BackupErrors        *prometheus.CounterVec
	BackupLastError     *prometheus.GaugeVec
	ConsecutiveFailures *prometheus.GaugeVec

	ConfigBytes        *prometheus.GaugeVec
	ConfigLines        *prometheus.GaugeVec
	ConfigChangedTotal *prometheus.CounterVec
	ConfigLastChanged  *prometheus.GaugeVec
	DeviceLastCommit   *prometheus.GaugeVec
	DeviceLastCommitBy *prometheus.GaugeVec

	GitOperations     *prometheus.CounterVec
	GitCommitsTotal   *prometheus.CounterVec
	GitLastCommit     prometheus.Gauge
	GitLastPush       prometheus.Gauge
	GitPushEnabled    prometheus.Gauge
	GitPushSuccess    prometheus.Gauge
	GitUnpushedCommit prometheus.Gauge
	GitRepoDirty      prometheus.Gauge
	GitLastError      *prometheus.GaugeVec

	RunTotal       *prometheus.CounterVec
	RunLast        prometheus.Gauge
	RunDuration    prometheus.Gauge
	RunInProgress  prometheus.Gauge
	DevicesTotal   prometheus.Gauge
	DevicesEnabled prometheus.Gauge
	DevicesFailing prometheus.Gauge
}

func gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: name, Help: help,
	}, labels)
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: name, Help: help,
	}, labels)
}

func plainGauge(name, help string) prometheus.Gauge {
	return prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: name, Help: help,
	})
}

// New builds and registers every collector.
func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),

		BuildInfo:  gauge("build_info", "Build metadata, always 1.", "version", "commit", "go_version"),
		ConfigLoad: gauge("config_load_success", "Whether the last config/inventory load succeeded.", "path"),

		DeviceInfo: gauge("device_info",
			"Device metadata as reported by the device, always 1.",
			"device", "host", "group", "transport", "model", "os_version"),

		BackupSuccess: gauge("backup_success",
			"Whether the last backup attempt for this device succeeded.", "device"),
		BackupLastAttempt: gauge("backup_last_attempt_timestamp_seconds",
			"Unix timestamp of the last backup attempt.", "device"),
		BackupLastSuccess: gauge("backup_last_success_timestamp_seconds",
			"Unix timestamp of the last successful backup.", "device"),
		BackupLastDuration: gauge("backup_last_duration_seconds",
			"Duration of the last backup attempt.", "device"),
		BackupDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "backup_duration_seconds",
			Help:      "Backup duration distribution.",
			Buckets:   []float64{0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300},
		}, []string{"device"}),
		BackupTotal: counter("backup_attempts_total",
			"Backup attempts by outcome.", "device", "result"),
		BackupErrors: counter("backup_errors_total",
			"Backup failures by the stage they failed in.", "device", "stage"),
		BackupLastError: gauge("backup_last_error",
			"Set to 1 for the stage the last backup failed in, absent when the device is healthy.",
			"device", "stage"),
		ConsecutiveFailures: gauge("backup_consecutive_failures",
			"Consecutive failed backups for this device.", "device"),

		ConfigBytes: gauge("config_bytes",
			"Size of the last fetched configuration.", "device", "format"),
		ConfigLines: gauge("config_lines",
			"Line count of the last fetched configuration.", "device", "format"),
		ConfigChangedTotal: counter("config_changed_total",
			"Times the stored configuration changed.", "device"),
		ConfigLastChanged: gauge("config_last_changed_timestamp_seconds",
			"Unix timestamp of the last configuration change committed for this device.", "device"),
		DeviceLastCommit: gauge("device_last_commit_timestamp_seconds",
			"Unix timestamp of the last commit made on the device itself.", "device"),
		DeviceLastCommitBy: gauge("device_last_commit_by",
			"Who made the last commit on the device, always 1.", "device", "user"),

		GitOperations: counter("git_operations_total",
			"Git operations by type and outcome.", "operation", "result"),
		GitCommitsTotal: counter("git_commits_total",
			"Commits written per device.", "device"),
		GitLastCommit: plainGauge("git_last_commit_timestamp_seconds",
			"Unix timestamp of the newest commit in the repository."),
		GitLastPush: plainGauge("git_last_push_success_timestamp_seconds",
			"Unix timestamp of the last successful push."),
		GitPushEnabled: plainGauge("git_push_enabled",
			"Whether pushing to a remote is configured."),
		GitPushSuccess: plainGauge("git_push_success",
			"Whether the last push attempt succeeded."),
		GitUnpushedCommit: plainGauge("git_unpushed_commits",
			"Commits present locally but not on the remote."),
		GitRepoDirty: plainGauge("git_repo_dirty",
			"Whether the repository has uncommitted changes."),
		GitLastError: gauge("git_last_error_timestamp_seconds",
			"Unix timestamp of the last failure of this git operation.", "operation"),

		RunTotal: counter("run_total",
			"Completed backup runs by outcome.", "result"),
		RunLast: plainGauge("run_last_timestamp_seconds",
			"Unix timestamp of the last completed run."),
		RunDuration: plainGauge("run_last_duration_seconds",
			"Duration of the last completed run."),
		RunInProgress: plainGauge("run_in_progress",
			"Whether a backup run is currently executing."),
		DevicesTotal: plainGauge("devices_total",
			"Devices in the inventory."),
		DevicesEnabled: plainGauge("devices_enabled",
			"Devices eligible for backup."),
		DevicesFailing: plainGauge("devices_failing",
			"Devices whose last backup attempt failed."),
	}

	m.Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.BuildInfo, m.ConfigLoad,
		m.DeviceInfo, m.BackupSuccess, m.BackupLastAttempt, m.BackupLastSuccess,
		m.BackupLastDuration, m.BackupDuration, m.BackupTotal, m.BackupErrors,
		m.BackupLastError, m.ConsecutiveFailures,
		m.ConfigBytes, m.ConfigLines, m.ConfigChangedTotal, m.ConfigLastChanged,
		m.DeviceLastCommit, m.DeviceLastCommitBy,
		m.GitOperations, m.GitCommitsTotal, m.GitLastCommit, m.GitLastPush,
		m.GitPushEnabled, m.GitPushSuccess, m.GitUnpushedCommit, m.GitRepoDirty,
		m.GitLastError,
		m.RunTotal, m.RunLast, m.RunDuration, m.RunInProgress,
		m.DevicesTotal, m.DevicesEnabled, m.DevicesFailing,
	)
	return m
}

// SetBuildInfo records build metadata.
func (m *Metrics) SetBuildInfo(version, commit, goVersion string) {
	m.BuildInfo.WithLabelValues(version, commit, goVersion).Set(1)
}

// ObserveAttempt records the start-of-run bookkeeping for a device.
func (m *Metrics) ObserveAttempt(device string, at time.Time) {
	m.BackupLastAttempt.WithLabelValues(device).Set(float64(at.Unix()))
}

// ObserveSuccess records a successful backup and clears the device's error state.
func (m *Metrics) ObserveSuccess(device string, at time.Time, dur time.Duration) {
	m.BackupSuccess.WithLabelValues(device).Set(1)
	m.BackupLastSuccess.WithLabelValues(device).Set(float64(at.Unix()))
	m.BackupLastDuration.WithLabelValues(device).Set(dur.Seconds())
	m.BackupDuration.WithLabelValues(device).Observe(dur.Seconds())
	m.BackupTotal.WithLabelValues(device, ResultSuccess).Inc()
	m.ConsecutiveFailures.WithLabelValues(device).Set(0)
	m.BackupLastError.DeletePartialMatch(prometheus.Labels{"device": device})
}

// ObserveFailure records a failed backup, labelled with the stage that failed.
func (m *Metrics) ObserveFailure(device, stage string, dur time.Duration, consecutive int) {
	m.BackupSuccess.WithLabelValues(device).Set(0)
	m.BackupLastDuration.WithLabelValues(device).Set(dur.Seconds())
	m.BackupDuration.WithLabelValues(device).Observe(dur.Seconds())
	m.BackupTotal.WithLabelValues(device, ResultFailure).Inc()
	m.BackupErrors.WithLabelValues(device, stage).Inc()
	m.ConsecutiveFailures.WithLabelValues(device).Set(float64(consecutive))
	m.BackupLastError.DeletePartialMatch(prometheus.Labels{"device": device})
	m.BackupLastError.WithLabelValues(device, stage).Set(1)
}

// ObserveGit records the outcome of a git operation.
func (m *Metrics) ObserveGit(op string, err error) {
	if err != nil {
		m.GitOperations.WithLabelValues(op, ResultFailure).Inc()
		m.GitLastError.WithLabelValues(op).Set(float64(time.Now().Unix()))
		return
	}
	m.GitOperations.WithLabelValues(op, ResultSuccess).Inc()
}

// ForgetDevice drops all series for a device that left the inventory.
func (m *Metrics) ForgetDevice(device string) {
	l := prometheus.Labels{"device": device}
	for _, v := range []interface{ DeletePartialMatch(prometheus.Labels) int }{
		m.DeviceInfo, m.BackupSuccess, m.BackupLastAttempt, m.BackupLastSuccess,
		m.BackupLastDuration, m.BackupDuration, m.BackupTotal, m.BackupErrors,
		m.BackupLastError, m.ConsecutiveFailures, m.ConfigBytes, m.ConfigLines,
		m.ConfigChangedTotal, m.ConfigLastChanged, m.DeviceLastCommit,
		m.DeviceLastCommitBy, m.GitCommitsTotal,
	} {
		v.DeletePartialMatch(l)
	}
}

// Bool converts a boolean to a gauge value.
func Bool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

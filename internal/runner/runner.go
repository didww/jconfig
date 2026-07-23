// Package runner schedules backups, stores results and updates metrics.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/junos"
	"github.com/didww/jconfig/internal/metrics"
	"github.com/didww/jconfig/internal/store"
)

// State is the live status of one device.
type State struct {
	Name        string          `json:"name"`
	Host        string          `json:"host"`
	Group       string          `json:"group,omitempty"`
	Transport   string          `json:"transport"`
	Enabled     bool            `json:"enabled"`
	Interval    config.Duration `json:"interval"`
	Hostname    string          `json:"hostname,omitempty"`
	Model       string          `json:"model,omitempty"`
	OSVersion   string          `json:"os_version,omitempty"`
	LastAttempt *time.Time      `json:"last_attempt,omitempty"`
	LastSuccess *time.Time      `json:"last_success,omitempty"`
	LastChanged *time.Time      `json:"last_changed,omitempty"`
	NextRun     *time.Time      `json:"next_run,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	LastStage   string          `json:"last_stage,omitempty"`
	Failures    int             `json:"consecutive_failures"`
	LastCommit  string          `json:"last_commit,omitempty"`
	// DeviceCommit is the last commit made on the device itself.
	DeviceCommit   *time.Time    `json:"device_last_commit,omitempty"`
	DeviceCommitBy string        `json:"device_last_commit_by,omitempty"`
	Duration       time.Duration `json:"-"`
	Running        bool          `json:"running"`
}

// DeviceResult is the outcome of one device backup.
type DeviceResult struct {
	Device   string        `json:"device"`
	OK       bool          `json:"ok"`
	Changed  bool          `json:"changed"`
	Commit   string        `json:"commit,omitempty"`
	Stage    string        `json:"stage,omitempty"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

// RunSummary is the outcome of a full or partial run.
type RunSummary struct {
	Started   time.Time      `json:"started"`
	Duration  time.Duration  `json:"duration_ns"`
	Total     int            `json:"total"`
	Succeeded int            `json:"succeeded"`
	Failed    int            `json:"failed"`
	Changed   int            `json:"changed"`
	Pushed    bool           `json:"pushed"`
	PushError string         `json:"push_error,omitempty"`
	Devices   []DeviceResult `json:"devices"`
}

// ErrBusy is returned when a run is requested while one is already going.
var ErrBusy = errors.New("a backup run is already in progress")

// Runner owns the device state table and drives backups.
type Runner struct {
	store *store.Store
	m     *metrics.Metrics
	log   *slog.Logger

	mu     sync.RWMutex
	cfg    *config.Config
	states map[string]*State

	runMu sync.Mutex
}

// New creates a runner for the given configuration.
func New(cfg *config.Config, st *store.Store, m *metrics.Metrics, log *slog.Logger) *Runner {
	r := &Runner{store: st, m: m, log: log, states: map[string]*State{}}
	r.Reload(cfg)
	return r
}

// Reload swaps in a new configuration, keeping the state of devices that
// survived and dropping metrics for those that did not.
func (r *Runner) Reload(cfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	old := r.states
	states := make(map[string]*State, len(cfg.Devices))

	for i := range cfg.Devices {
		d := &cfg.Devices[i]
		st, ok := old[d.Name]
		if !ok {
			st = &State{Name: d.Name}
			next := now.Add(r.startDelay(cfg))
			if !*cfg.Scheduler.RunOnStart {
				next = now.Add(d.Interval.Duration())
			}
			st.NextRun = &next
		}
		st.Host = d.Host
		st.Group = d.Group
		st.Transport = d.Transport
		st.Enabled = d.IsEnabled()
		st.Interval = d.Interval
		states[d.Name] = st
	}

	for name := range old {
		if _, ok := states[name]; !ok {
			r.m.ForgetDevice(name)
			r.log.Info("device removed from inventory", "device", name)
		}
	}

	r.cfg = cfg
	r.states = states

	r.m.DevicesTotal.Set(float64(len(cfg.Devices)))
	r.m.DevicesEnabled.Set(float64(len(cfg.EnabledDevices())))
	r.m.GitPushEnabled.Set(metrics.Bool(cfg.Repo.Push.Enabled))
	r.m.ConfigLoad.WithLabelValues(cfg.Path).Set(1)
}

// startDelay spreads the initial run of each device over the jitter window.
func (r *Runner) startDelay(cfg *config.Config) time.Duration {
	j := cfg.Scheduler.Jitter.Duration()
	if j <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(j)))
}

// repo returns the active repository.
func (r *Runner) repo() *store.Store {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store
}

// SetStore swaps the repository, used when repo settings change on reload.
func (r *Runner) SetStore(st *store.Store) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = st
}

// Ready reports whether jconfig can do its job: the repository must be
// readable. Used by the readiness probe.
func (r *Runner) Ready() error {
	if _, _, err := r.repo().HeadCommit(); err != nil {
		return fmt.Errorf("repository not readable: %w", err)
	}
	return nil
}

// Config returns the active configuration.
func (r *Runner) Config() *config.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// States returns a snapshot of every device's status, sorted by name.
func (r *Runner) States() []State {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]State, 0, len(r.states))
	for _, st := range r.states {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Serve runs the scheduler until ctx is cancelled.
func (r *Runner) Serve(ctx context.Context) {
	cfg := r.Config()
	scan := cfg.Scheduler.ScanInterval.Duration()
	r.log.Info("scheduler started",
		"devices", len(cfg.Devices),
		"interval", cfg.Scheduler.Interval,
		"concurrency", cfg.Scheduler.Concurrency,
		"scan_interval", cfg.Scheduler.ScanInterval)

	ticker := time.NewTicker(scan)
	defer ticker.Stop()

	// Check once up front so run_on_start does not wait out a scan interval.
	r.runDue(ctx)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("scheduler stopped")
			return
		case <-ticker.C:
			r.runDue(ctx)
		}
	}
}

// runDue backs up every device whose next run is in the past.
func (r *Runner) runDue(ctx context.Context) {
	due := r.dueDevices(time.Now())
	if len(due) == 0 {
		return
	}
	if _, err := r.run(ctx, due); err != nil && !errors.Is(err, ErrBusy) {
		r.log.Error("run failed", "err", err)
	}
}

func (r *Runner) dueDevices(now time.Time) []config.Device {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var due []config.Device
	for i := range r.cfg.Devices {
		d := r.cfg.Devices[i]
		if !d.IsEnabled() {
			continue
		}
		st := r.states[d.Name]
		if st == nil || st.Running {
			continue
		}
		if st.NextRun == nil || !st.NextRun.After(now) {
			due = append(due, d)
		}
	}
	return due
}

// RunAll backs up every enabled device now.
func (r *Runner) RunAll(ctx context.Context) (*RunSummary, error) {
	r.mu.RLock()
	devs := r.cfg.EnabledDevices()
	r.mu.RUnlock()
	return r.run(ctx, devs)
}

// RunDevice backs up a single device by name.
func (r *Runner) RunDevice(ctx context.Context, name string) (*RunSummary, error) {
	r.mu.RLock()
	var found *config.Device
	for i := range r.cfg.Devices {
		if r.cfg.Devices[i].Name == name {
			found = &r.cfg.Devices[i]
			break
		}
	}
	r.mu.RUnlock()

	if found == nil {
		return nil, fmt.Errorf("unknown device %q", name)
	}
	return r.run(ctx, []config.Device{*found})
}

// run backs up the given devices with the configured concurrency. Only one run
// executes at a time; overlapping requests get ErrBusy.
func (r *Runner) run(ctx context.Context, devs []config.Device) (*RunSummary, error) {
	if !r.runMu.TryLock() {
		return nil, ErrBusy
	}
	defer r.runMu.Unlock()

	if len(devs) == 0 {
		return &RunSummary{Started: time.Now()}, nil
	}

	cfg := r.Config()
	started := time.Now()
	r.m.RunInProgress.Set(1)
	defer r.m.RunInProgress.Set(0)

	r.log.Info("run started", "devices", len(devs))

	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, cfg.Scheduler.Concurrency)
		resMu   sync.Mutex
		results = make([]DeviceResult, 0, len(devs))
	)

	for i := range devs {
		d := devs[i]
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			res := r.backup(ctx, &d)

			resMu.Lock()
			results = append(results, res)
			resMu.Unlock()
		}()
	}
	wg.Wait()

	sum := &RunSummary{
		Started:  started,
		Duration: time.Since(started),
		Total:    len(results),
		Devices:  results,
	}
	for _, res := range results {
		switch {
		case res.OK:
			sum.Succeeded++
		default:
			sum.Failed++
		}
		if res.Changed {
			sum.Changed++
		}
	}
	sort.Slice(sum.Devices, func(i, j int) bool { return sum.Devices[i].Device < sum.Devices[j].Device })

	r.syncRepoMetrics(ctx, sum)

	r.m.RunLast.Set(float64(time.Now().Unix()))
	r.m.RunDuration.Set(sum.Duration.Seconds())
	if sum.Failed > 0 {
		r.m.RunTotal.WithLabelValues(metrics.ResultFailure).Inc()
	} else {
		r.m.RunTotal.WithLabelValues(metrics.ResultSuccess).Inc()
	}
	r.m.DevicesFailing.Set(float64(r.failingCount()))

	r.log.Info("run finished",
		"devices", sum.Total,
		"ok", sum.Succeeded,
		"failed", sum.Failed,
		"changed", sum.Changed,
		"duration", sum.Duration.Round(time.Millisecond))
	return sum, nil
}

// backup fetches one device and commits the result.
func (r *Runner) backup(ctx context.Context, d *config.Device) DeviceResult {
	start := time.Now()
	res := DeviceResult{Device: d.Name}

	r.setRunning(d.Name, true)
	defer r.setRunning(d.Name, false)

	r.m.ObserveAttempt(d.Name, start)
	r.update(d.Name, func(st *State) {
		t := start
		st.LastAttempt = &t
	})

	fetched, err := junos.Fetch(ctx, d)
	if err != nil {
		stage := junos.StageOf(err)
		res.Stage = stage
		res.Error = err.Error()
		res.Duration = time.Since(start)

		// Compute the retry delay before taking the write lock: it reads
		// the config under the same mutex.
		next := time.Now().Add(r.retryDelay(d))
		failures := 0
		r.update(d.Name, func(st *State) {
			st.Failures++
			st.LastError = err.Error()
			st.LastStage = stage
			st.Duration = res.Duration
			failures = st.Failures
			st.NextRun = &next
		})
		r.m.ObserveFailure(d.Name, stage, res.Duration, failures)
		r.log.Error("backup failed",
			"device", d.Name, "host", d.Host, "stage", stage, "err", err)
		return res
	}

	files := make(map[string]string, len(fetched.Configs))
	for format, content := range fetched.Configs {
		files[repoPath(r.Config().Repo.Layout, d, format)] = content
		r.m.ConfigBytes.WithLabelValues(d.Name, format).Set(float64(len(content)))
		r.m.ConfigLines.WithLabelValues(d.Name, format).Set(float64(countLines(content)))
	}

	commit, cerr := r.repo().Commit(store.CommitRequest{
		Device:  d.Name,
		Files:   files,
		Message: commitMessage(r.Config().Repo.CommitPrefix, d, fetched, files),
		When:    time.Now(),
	})
	r.m.ObserveGit(metrics.OpCommit, cerr)
	if cerr != nil {
		res.Stage = "git"
		res.Error = cerr.Error()
		res.Duration = time.Since(start)

		next := time.Now().Add(r.retryDelay(d))
		failures := 0
		r.update(d.Name, func(st *State) {
			st.Failures++
			st.LastError = cerr.Error()
			st.LastStage = "git"
			failures = st.Failures
			st.NextRun = &next
		})
		// A git failure is a backup failure: the config was fetched but
		// never stored.
		r.m.ObserveFailure(d.Name, "git", res.Duration, failures)
		r.log.Error("commit failed", "device", d.Name, "err", cerr)
		return res
	}

	now := time.Now()
	res.OK = true
	res.Changed = commit.Changed
	res.Commit = commit.Hash
	res.Duration = time.Since(start)

	if commit.Changed {
		r.m.ConfigChangedTotal.WithLabelValues(d.Name).Inc()
		r.m.ConfigLastChanged.WithLabelValues(d.Name).Set(float64(now.Unix()))
		r.m.GitCommitsTotal.WithLabelValues(d.Name).Inc()
	}
	r.m.ObserveSuccess(d.Name, now, res.Duration)
	r.m.DeviceInfo.DeletePartialMatch(map[string]string{"device": d.Name})
	r.m.DeviceInfo.WithLabelValues(
		d.Name, d.Host, d.Group, d.Transport, fetched.Model, fetched.OSVersion).Set(1)
	if !fetched.LastCommit.IsZero() {
		r.m.DeviceLastCommit.WithLabelValues(d.Name).Set(float64(fetched.LastCommit.Unix()))
	}
	if fetched.LastCommitBy != "" {
		r.m.DeviceLastCommitBy.DeletePartialMatch(map[string]string{"device": d.Name})
		r.m.DeviceLastCommitBy.WithLabelValues(d.Name, fetched.LastCommitBy).Set(1)
	}

	r.update(d.Name, func(st *State) {
		t := now
		st.LastSuccess = &t
		st.LastError = ""
		st.LastStage = ""
		st.Failures = 0
		st.Duration = res.Duration
		st.Hostname = fetched.Hostname
		st.Model = fetched.Model
		st.OSVersion = fetched.OSVersion
		st.DeviceCommitBy = fetched.LastCommitBy
		if !fetched.LastCommit.IsZero() {
			dc := fetched.LastCommit
			st.DeviceCommit = &dc
		}
		if commit.Changed {
			ct := now
			st.LastChanged = &ct
			st.LastCommit = commit.Hash
		}
		next := now.Add(d.Interval.Duration())
		st.NextRun = &next
	})

	if commit.Changed {
		r.log.Info("config changed",
			"device", d.Name, "commit", short(commit.Hash), "files", strings.Join(commit.Files, ", "))
	} else {
		r.log.Debug("config unchanged", "device", d.Name,
			"duration", res.Duration.Round(time.Millisecond))
	}
	return res
}

// syncRepoMetrics pushes when configured and refreshes repository gauges.
func (r *Runner) syncRepoMetrics(ctx context.Context, sum *RunSummary) {
	if t, _, err := r.repo().HeadCommit(); err == nil {
		if !t.IsZero() {
			r.m.GitLastCommit.Set(float64(t.Unix()))
		}
	} else {
		r.m.ObserveGit(metrics.OpStatus, err)
		r.log.Error("read HEAD failed", "err", err)
	}

	if dirty, err := r.repo().Dirty(); err == nil {
		r.m.GitRepoDirty.Set(metrics.Bool(dirty))
		if dirty {
			r.log.Warn("repository has uncommitted changes")
		}
	} else {
		r.m.ObserveGit(metrics.OpStatus, err)
	}

	cfg := r.Config()
	if !cfg.Repo.Push.Enabled {
		return
	}

	pushed, err := r.repo().Push(ctx)
	r.m.ObserveGit(metrics.OpPush, err)
	if err != nil {
		r.m.GitPushSuccess.Set(0)
		sum.PushError = err.Error()
		r.log.Error("push failed", "remote", cfg.Repo.Push.Remote, "err", err)
	} else {
		r.m.GitPushSuccess.Set(1)
		r.m.GitLastPush.Set(float64(time.Now().Unix()))
		sum.Pushed = pushed
		if pushed {
			r.log.Info("pushed", "remote", cfg.Repo.Push.Remote, "branch", cfg.Repo.Push.Branch)
		}
	}

	ahead, err := r.repo().Unpushed(ctx)
	if err != nil {
		r.m.ObserveGit(metrics.OpStatus, err)
		r.log.Warn("could not determine unpushed commits", "err", err)
		return
	}
	r.m.GitUnpushedCommit.Set(float64(ahead))
}

func (r *Runner) retryDelay(d *config.Device) time.Duration {
	cfg := r.Config()
	if v := cfg.Scheduler.RetryInterval.Duration(); v > 0 {
		return v
	}
	return d.Interval.Duration()
}

// update mutates one device's state under the write lock. fn must not call
// back into the runner: every accessor takes the same mutex.
func (r *Runner) update(name string, fn func(*State)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.states[name]; ok {
		fn(st)
	}
}

func (r *Runner) setRunning(name string, running bool) {
	r.update(name, func(st *State) { st.Running = running })
}

func (r *Runner) failingCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := 0
	for _, st := range r.states {
		if st.Failures > 0 {
			n++
		}
	}
	return n
}

// repoPath is where a device's config of a given format lives in the repo.
func repoPath(layout string, d *config.Device, format string) string {
	name := sanitizeName(d.Name) + config.Extensions[format]
	if layout == "group" {
		group := sanitizeName(d.Group)
		if group == "" {
			group = "ungrouped"
		}
		return group + "/" + name
	}
	return name
}

// sanitizeName keeps device names from escaping the repository directory.
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, `\`, "_")
	s = strings.ReplaceAll(s, "..", "_")
	return strings.Trim(s, ". ")
}

func commitMessage(prefix string, d *config.Device, res *junos.Result, files map[string]string) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(d.Name)
	b.WriteString(": configuration changed\n\n")

	fmt.Fprintf(&b, "device:    %s (%s)\n", d.Name, d.Host)
	if res.Hostname != "" && res.Hostname != d.Name {
		fmt.Fprintf(&b, "hostname:  %s\n", res.Hostname)
	}
	if res.Model != "" {
		fmt.Fprintf(&b, "model:     %s\n", res.Model)
	}
	if res.OSVersion != "" {
		fmt.Fprintf(&b, "junos:     %s\n", res.OSVersion)
	}
	fmt.Fprintf(&b, "transport: %s\n", d.Transport)
	if !res.LastCommit.IsZero() {
		by := res.LastCommitBy
		if by == "" {
			by = "unknown"
		}
		fmt.Fprintf(&b, "committed on device: %s by %s\n",
			res.LastCommit.UTC().Format(time.RFC3339), by)
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	b.WriteString("files:\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "  %s\n", p)
	}
	return b.String()
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

func short(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

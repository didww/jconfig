package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/junostest"
	"github.com/didww/jconfig/internal/metrics"
	"github.com/didww/jconfig/internal/store"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type harness struct {
	r    *Runner
	m    *metrics.Metrics
	st   *store.Store
	dev1 *junostest.Server
	dev2 *junostest.Server
	repo string
}

// newHarness wires two fake Junos devices to a runner backed by a temporary
// git repository.
func newHarness(t *testing.T, layout string) *harness {
	t.Helper()

	dev1 := junostest.Start(t)
	dev2 := junostest.Start(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "configs")
	cfgPath := filepath.Join(dir, "jconfig.yml")

	cfgYAML := fmt.Sprintf(`
repo:
  path: %s
  layout: %s
scheduler:
  interval: 1h
  concurrency: 4
defaults:
  username: %s
  password: %s
  insecure_ignore_host_key: true
  remove_lines:
    - '^## Last commit:'
devices:
  - name: mx1
    host: %s
    port: %d
    group: core
    transport: ssh
  - name: mx2
    host: %s
    port: %d
    group: edge
    transport: netconf
`, repo, layout, junostest.User, junostest.Pass,
		dev1.Host(), dev1.Port(), dev2.Host(), dev2.Port())

	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	m := metrics.New()
	st, err := store.Open(context.Background(), cfg.Repo)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &harness{
		r: New(cfg, st, m, log), m: m, st: st,
		dev1: dev1, dev2: dev2, repo: repo,
	}
}

func TestRunAllStoresBothDevices(t *testing.T) {
	h := newHarness(t, "flat")

	sum, err := h.r.RunAll(context.Background())
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if sum.Total != 2 || sum.Succeeded != 2 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want 2 successes", sum)
	}
	if sum.Changed != 2 {
		t.Errorf("Changed = %d, want 2 on the first run", sum.Changed)
	}

	for _, name := range []string{"mx1.conf", "mx1.set", "mx2.conf", "mx2.set"} {
		p := filepath.Join(h.repo, name)
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("expected %s in the repo: %v", name, err)
		}
		if !strings.Contains(string(body), "mx1-ams") {
			t.Errorf("%s does not look like a Junos config:\n%s", name, body)
		}
		// remove_lines must have stripped the volatile header.
		if strings.Contains(string(body), "## Last commit:") {
			t.Errorf("%s still contains the Last commit header", name)
		}
	}

	// One commit per changed device.
	if got := testutil.ToFloat64(h.m.GitCommitsTotal.WithLabelValues("mx1")); got != 1 {
		t.Errorf("git_commits_total{mx1} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.m.BackupSuccess.WithLabelValues("mx1")); got != 1 {
		t.Errorf("backup_success{mx1} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.m.BackupSuccess.WithLabelValues("mx2")); got != 1 {
		t.Errorf("backup_success{mx2} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.m.DevicesFailing); got != 0 {
		t.Errorf("devices_failing = %v, want 0", got)
	}
	// Metadata scraped from the device.
	if got := testutil.ToFloat64(h.m.DeviceLastCommit.WithLabelValues("mx1")); got != 1784894400 {
		t.Errorf("device_last_commit_timestamp_seconds{mx1} = %v", got)
	}
}

func TestRerunWithoutChangesMakesNoCommit(t *testing.T) {
	h := newHarness(t, "flat")

	if _, err := h.r.RunAll(context.Background()); err != nil {
		t.Fatalf("first RunAll: %v", err)
	}
	sum, err := h.r.RunAll(context.Background())
	if err != nil {
		t.Fatalf("second RunAll: %v", err)
	}
	if sum.Failed != 0 {
		t.Fatalf("second run failed: %+v", sum)
	}
	if sum.Changed != 0 {
		t.Errorf("Changed = %d, want 0 when nothing moved on the devices", sum.Changed)
	}
	if got := testutil.ToFloat64(h.m.ConfigChangedTotal.WithLabelValues("mx1")); got != 1 {
		t.Errorf("config_changed_total{mx1} = %v, want 1", got)
	}
}

func TestChangedConfigProducesCommit(t *testing.T) {
	h := newHarness(t, "flat")

	if _, err := h.r.RunAll(context.Background()); err != nil {
		t.Fatalf("first RunAll: %v", err)
	}

	h.dev1.SetText(junostest.TextConfig + "protocols {\n    bgp;\n}\n")

	sum, err := h.r.RunAll(context.Background())
	if err != nil {
		t.Fatalf("second RunAll: %v", err)
	}
	if sum.Changed != 1 {
		t.Fatalf("Changed = %d, want 1", sum.Changed)
	}

	body, err := os.ReadFile(filepath.Join(h.repo, "mx1.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "bgp;") {
		t.Errorf("the new configuration was not stored:\n%s", body)
	}
	if got := testutil.ToFloat64(h.m.ConfigChangedTotal.WithLabelValues("mx1")); got != 2 {
		t.Errorf("config_changed_total{mx1} = %v, want 2", got)
	}
}

func TestGroupLayout(t *testing.T) {
	h := newHarness(t, "group")

	if _, err := h.r.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	for _, p := range []string{"core/mx1.conf", "edge/mx2.conf"} {
		if _, err := os.Stat(filepath.Join(h.repo, p)); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}
}

func TestFailureIsRecorded(t *testing.T) {
	h := newHarness(t, "flat")
	h.dev1.SetFailCommand("show configuration")

	sum, err := h.r.RunAll(context.Background())
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if sum.Failed != 1 || sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want one failure and one success", sum)
	}

	if got := testutil.ToFloat64(h.m.BackupSuccess.WithLabelValues("mx1")); got != 0 {
		t.Errorf("backup_success{mx1} = %v, want 0", got)
	}
	if got := testutil.ToFloat64(h.m.ConsecutiveFailures.WithLabelValues("mx1")); got != 1 {
		t.Errorf("backup_consecutive_failures{mx1} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.m.BackupErrors.WithLabelValues("mx1", "parse")); got != 1 {
		t.Errorf("backup_errors_total{mx1,parse} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.m.BackupLastError.WithLabelValues("mx1", "parse")); got != 1 {
		t.Errorf("backup_last_error{mx1,parse} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.m.DevicesFailing); got != 1 {
		t.Errorf("devices_failing = %v, want 1", got)
	}

	// A failed device must not leave a partial file behind.
	if _, err := os.Stat(filepath.Join(h.repo, "mx1.conf")); !os.IsNotExist(err) {
		t.Errorf("a failed backup must not write a config file (err=%v)", err)
	}

	// State reflects the failure.
	var st *State
	for _, s := range h.r.States() {
		if s.Name == "mx1" {
			s := s
			st = &s
		}
	}
	if st == nil || st.Failures != 1 || st.LastStage != "parse" {
		t.Fatalf("state = %+v, want one parse failure", st)
	}
}

func TestRecoveryClearsErrorState(t *testing.T) {
	h := newHarness(t, "flat")
	h.dev1.SetFailCommand("show configuration")

	if _, err := h.r.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	h.dev1.SetFailCommand("")

	sum, err := h.r.RunAll(context.Background())
	if err != nil {
		t.Fatalf("second RunAll: %v", err)
	}
	if sum.Failed != 0 {
		t.Fatalf("summary = %+v, want a clean run", sum)
	}
	if got := testutil.ToFloat64(h.m.ConsecutiveFailures.WithLabelValues("mx1")); got != 0 {
		t.Errorf("backup_consecutive_failures{mx1} = %v, want 0 after recovery", got)
	}
	// The stale per-stage error series must be gone, not left at 1.
	if n := testutil.CollectAndCount(h.m.BackupLastError); n != 0 {
		t.Errorf("backup_last_error still has %d series after recovery", n)
	}
}

func TestRunDeviceUnknown(t *testing.T) {
	h := newHarness(t, "flat")

	if _, err := h.r.RunDevice(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error for an unknown device")
	}
	sum, err := h.r.RunDevice(context.Background(), "mx1")
	if err != nil {
		t.Fatalf("RunDevice: %v", err)
	}
	if sum.Total != 1 || sum.Succeeded != 1 {
		t.Errorf("summary = %+v, want a single successful device", sum)
	}
}

func TestReloadForgetsRemovedDevices(t *testing.T) {
	h := newHarness(t, "flat")

	if _, err := h.r.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if n := testutil.CollectAndCount(h.m.BackupSuccess); n != 2 {
		t.Fatalf("backup_success series = %d, want 2", n)
	}

	// Drop mx2 from the configuration.
	cfg := h.r.Config()
	trimmed := *cfg
	trimmed.Devices = cfg.Devices[:1]
	h.r.Reload(&trimmed)

	if n := testutil.CollectAndCount(h.m.BackupSuccess); n != 1 {
		t.Errorf("backup_success series = %d, want 1 after mx2 left the inventory", n)
	}
	if got := len(h.r.States()); got != 1 {
		t.Errorf("States() = %d, want 1", got)
	}
}

func TestRepoPath(t *testing.T) {
	d := &config.Device{Name: "mx1", Group: "core"}

	if got := repoPath("flat", d, config.FormatText); got != "mx1.conf" {
		t.Errorf("flat text path = %q", got)
	}
	if got := repoPath("group", d, config.FormatSet); got != "core/mx1.set" {
		t.Errorf("group set path = %q", got)
	}

	d.Group = ""
	if got := repoPath("group", d, config.FormatXML); got != "ungrouped/mx1.xml" {
		t.Errorf("ungrouped path = %q", got)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"mx1":   "mx1",
		"a/b":   "a_b",
		`a\b`:   "a_b",
		" mx1 ": "mx1",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeNameBlocksTraversal(t *testing.T) {
	// A device name reaches the filesystem as a path, so no name may keep a
	// separator or a parent reference, whatever it looked like going in.
	hostile := []string{
		"../../etc/passwd",
		`..\..\windows`,
		"/etc/passwd",
		"..",
		"...",
		"a/../../b",
	}
	for _, in := range hostile {
		got := sanitizeName(in)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitizeName(%q) = %q, still contains a separator", in, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("sanitizeName(%q) = %q, still contains \"..\"", in, got)
		}
		// The result must stay inside the repository.
		full := filepath.Join("/repo", got+".conf")
		if !strings.HasPrefix(full, "/repo/") {
			t.Errorf("sanitizeName(%q) = %q escapes the repository: %s", in, got, full)
		}
	}
}

func TestCountLines(t *testing.T) {
	cases := map[string]int{"": 0, "a\n": 1, "a\nb\n": 2, "a\nb": 2}
	for in, want := range cases {
		if got := countLines(in); got != want {
			t.Errorf("countLines(%q) = %d, want %d", in, got, want)
		}
	}
}

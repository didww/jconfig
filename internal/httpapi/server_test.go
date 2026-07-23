package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/metrics"
	"github.com/didww/jconfig/internal/runner"
	"github.com/didww/jconfig/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "jconfig.yml")
	cfgYAML := "repo:\n  path: " + filepath.Join(dir, "configs") + `
defaults:
  username: backup
  password: pw
  insecure_ignore_host_key: true
devices:
  - name: mx1
    host: 10.0.0.1
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
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
	r := runner.New(cfg, st, m, log)

	return New(Config{
		MetricsAddr:    "127.0.0.1:0",
		MetricsPath:    "/metrics",
		ManagementAddr: "127.0.0.1:0",
	}, r, m, log)
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// The whole point of the split: the scrape port must not expose the control
// surface.
func TestMetricsListenerRefusesManagementEndpoints(t *testing.T) {
	mux := testServer(t).metricsMux()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/backup"},
		{http.MethodGet, "/backup"},
		{http.MethodGet, "/devices"},
	} {
		rec := do(mux, tc.method, tc.path)
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s returned 200 on the metrics listener; it must not be served there",
				tc.method, tc.path)
		}
	}
}

func TestMetricsListenerServesMetricsAndProbes(t *testing.T) {
	mux := testServer(t).metricsMux()

	rec := do(mux, http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "jconfig_devices_total") {
		t.Error("exposition does not contain jconfig metrics")
	}

	if rec := do(mux, http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d", rec.Code)
	}
	if rec := do(mux, http.MethodGet, "/ready"); rec.Code != http.StatusOK {
		t.Errorf("GET /ready = %d, want 200 with a readable repository", rec.Code)
	}
}

func TestManagementListenerServesControlSurface(t *testing.T) {
	mux := testServer(t).managementMux()

	rec := do(mux, http.MethodGet, "/devices")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /devices = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "mx1") {
		t.Errorf("device list does not mention mx1: %s", rec.Body.String())
	}

	rec = do(mux, http.MethodGet, "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mx1") {
		t.Errorf("status page = %d: %s", rec.Code, rec.Body.String())
	}
}

// Metrics must not be reachable through the management socket either: one
// scrape target, one place to look.
func TestManagementListenerDoesNotServeMetrics(t *testing.T) {
	mux := testServer(t).managementMux()

	rec := do(mux, http.MethodGet, "/metrics")
	if strings.Contains(rec.Body.String(), "jconfig_devices_total") {
		t.Error("the management listener is serving the metrics exposition")
	}
}

func TestManagementCanBeDisabled(t *testing.T) {
	s := testServer(t)
	s.cfg.ManagementAddr = ""
	s2 := New(s.cfg, s.run, s.m, s.log)

	if s2.mgmt != nil {
		t.Error("an empty management_listen must not create a management server")
	}
}

func TestBackupRejectsUnknownDevice(t *testing.T) {
	mux := testServer(t).managementMux()

	rec := do(mux, http.MethodPost, "/backup?device=nope")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /backup?device=nope = %d, want 400", rec.Code)
	}
}

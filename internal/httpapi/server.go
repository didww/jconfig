// Package httpapi serves jconfig over two separate sockets: a metrics socket
// carrying only /metrics and the health probes, and a management socket
// carrying the control surface. Keeping them apart means the port a Prometheus
// server scrapes cannot be used to trigger backups or enumerate the inventory.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/didww/jconfig/internal/metrics"
	"github.com/didww/jconfig/internal/runner"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Config describes the two sockets.
type Config struct {
	// MetricsAddr serves /metrics, /healthz and /ready.
	MetricsAddr string
	// MetricsPath is where the exposition lives, normally /metrics.
	MetricsPath string
	// ManagementAddr serves the control surface. Empty disables it.
	ManagementAddr string
}

// Server owns both listeners.
type Server struct {
	cfg  Config
	run  *runner.Runner
	m    *metrics.Metrics
	log  *slog.Logger
	prom *http.Server
	mgmt *http.Server // nil when the control surface is disabled
}

// New builds the servers without binding anything yet.
func New(cfg Config, run *runner.Runner, m *metrics.Metrics, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, run: run, m: m, log: log}

	s.prom = &http.Server{
		Handler:           s.metricsMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if cfg.ManagementAddr != "" {
		s.mgmt = &http.Server{
			Handler:           s.managementMux(),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}
	return s
}

// metricsMux serves scrape and probe traffic, and nothing else.
func (s *Server) metricsMux() *http.ServeMux {
	path := s.cfg.MetricsPath
	if path == "" {
		path = "/metrics"
	}

	mux := http.NewServeMux()
	mux.Handle("GET "+path, promhttp.HandlerFor(s.m.Registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	}))
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)
	// Anything else, including the management endpoints, is not served here.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "jconfig metrics endpoint\n\n%s\n/healthz\n/ready\n", path)
	})
	return mux
}

// managementMux serves the control surface.
func (s *Server) managementMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /devices", s.handleDevices)
	mux.HandleFunc("POST /backup", s.handleBackup)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

// Serve binds both sockets and runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	promLn, err := net.Listen("tcp", s.cfg.MetricsAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.MetricsAddr, err)
	}
	s.log.Info("metrics listening", "addr", s.cfg.MetricsAddr, "path", s.cfg.MetricsPath)

	var mgmtLn net.Listener
	if s.mgmt != nil {
		mgmtLn, err = net.Listen("tcp", s.cfg.ManagementAddr)
		if err != nil {
			_ = promLn.Close()
			return fmt.Errorf("listen on %s: %w", s.cfg.ManagementAddr, err)
		}
		s.log.Info("management listening", "addr", s.cfg.ManagementAddr)
	} else {
		s.log.Info("management endpoint disabled")
	}

	errc := make(chan error, 2)
	var wg sync.WaitGroup

	serve := func(srv *http.Server, ln net.Listener) {
		defer wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}

	wg.Add(1)
	go serve(s.prom, promLn)
	if s.mgmt != nil {
		wg.Add(1)
		go serve(s.mgmt, mgmtLn)
	}

	select {
	case err := <-errc:
		s.shutdown()
		wg.Wait()
		return err
	case <-ctx.Done():
		s.shutdown()
		wg.Wait()
		return nil
	}
}

func (s *Server) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.prom.Shutdown(ctx); err != nil {
		s.log.Error("metrics server shutdown", "err", err)
	}
	if s.mgmt != nil {
		if err := s.mgmt.Shutdown(ctx); err != nil {
			s.log.Error("management server shutdown", "err", err)
		}
	}
}

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleReady reports whether jconfig can actually do its job, which means the
// repository is readable. A daemon that cannot reach its repository is up but
// useless, and should not be treated as healthy.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if err := s.run.Ready(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not ready",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) handleDevices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.run.States())
}

// handleBackup triggers a run. `?device=` limits it to one device; `?wait=false`
// returns immediately instead of blocking for the result.
func (s *Server) handleBackup(w http.ResponseWriter, req *http.Request) {
	device := req.URL.Query().Get("device")
	wait := req.URL.Query().Get("wait") != "false"

	do := func(ctx context.Context) (*runner.RunSummary, error) {
		if device != "" {
			return s.run.RunDevice(ctx, device)
		}
		return s.run.RunAll(ctx)
	}

	if !wait {
		go func() {
			// Detached from the request: use a fresh context so the run
			// survives the client disconnecting.
			if _, err := do(context.Background()); err != nil {
				s.log.Error("triggered run failed", "err", err)
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "device": device})
		return
	}

	sum, err := do(req.Context())
	switch {
	case errors.Is(err, runner.ErrBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, sum)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}

	states := s.run.States()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	var b strings.Builder
	b.WriteString("jconfig - network configuration backup\n\n")
	fmt.Fprintf(&b, "repository: %s\n", s.run.Config().Repo.Path)
	fmt.Fprintf(&b, "devices:    %d\n\n", len(states))

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tHOST\tSTATUS\tLAST SUCCESS\tLAST CHANGE\tNEXT RUN\tDETAIL")
	for _, st := range states {
		status := "ok"
		switch {
		case st.Running:
			status = "running"
		case !st.Enabled:
			status = "disabled"
		case st.Failures > 0:
			status = fmt.Sprintf("FAILED(%d)", st.Failures)
		case st.LastSuccess == nil:
			status = "pending"
		}
		detail := st.LastError
		if detail == "" {
			detail = strings.TrimSpace(st.Model + " " + st.OSVersion)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			st.Name, st.Host, status,
			ago(st.LastSuccess), ago(st.LastChanged), until(st.NextRun),
			truncate(detail, 60))
	}
	_ = tw.Flush()

	b.WriteString("\nendpoints: /devices  /healthz  POST /backup[?device=NAME][&wait=false]\n")
	fmt.Fprintf(&b, "metrics are served separately on %s\n", s.cfg.MetricsAddr)
	_, _ = w.Write([]byte(b.String()))
}

func ago(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return time.Since(*t).Round(time.Second).String() + " ago"
}

func until(t *time.Time) string {
	if t == nil {
		return "-"
	}
	d := time.Until(*t).Round(time.Second)
	if d <= 0 {
		return "due"
	}
	return "in " + d.String()
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

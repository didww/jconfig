// Command jconfig backs up Junos device configurations into a git repository
// and exports Prometheus metrics about the health of those backups.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/httpapi"
	"github.com/didww/jconfig/internal/metrics"
	"github.com/didww/jconfig/internal/runner"
	"github.com/didww/jconfig/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// Set with -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "jconfig: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "/etc/jconfig/jconfig.yml", "path to the configuration file")
		once        = flag.Bool("once", false, "back up every device once, then exit (non-zero exit if any device failed)")
		check       = flag.Bool("check", false, "validate the configuration and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
		logLevel    = flag.String("log-level", "", "override log_level (debug, info, warn, error)")
		metricsFile = flag.String("metrics-file", "", "with -once, write metrics to this file for the Prometheus textfile collector")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("jconfig %s (%s) %s\n", version, commit, runtime.Version())
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}

	if *check {
		fmt.Printf("%s: ok, %d devices (%d enabled)\n",
			*configPath, len(cfg.Devices), len(cfg.EnabledDevices()))
		return nil
	}

	log := newLogger(cfg)
	m := metrics.New()
	m.SetBuildInfo(version, commit, runtime.Version())

	// Opening may clone from the push remote, so give it its own context
	// that a shutdown signal can cancel.
	openCtx, cancelOpen := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	st, err := store.Open(openCtx, cfg.Repo)
	cancelOpen()
	m.ObserveGit(metrics.OpOpen, err)
	if err != nil {
		return err
	}
	log.Info("repository ready", "path", cfg.Repo.Path, "branch", cfg.Repo.Branch,
		"push", cfg.Repo.Push.Enabled)

	r := runner.New(cfg, st, m, log)

	if *once {
		return runOnce(r, m, log, *metricsFile)
	}
	return serve(cfg, r, m, log, *configPath)
}

// runOnce performs a single pass over every device, prints a summary and
// reports failure through the exit status.
func runOnce(r *runner.Runner, m *metrics.Metrics, log *slog.Logger, metricsFile string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sum, err := r.RunAll(ctx)
	if err != nil {
		return err
	}
	printSummary(sum)

	if metricsFile != "" {
		if err := writeMetricsFile(m.Registry, metricsFile); err != nil {
			log.Error("write metrics file", "path", metricsFile, "err", err)
			return err
		}
	}
	if sum.Failed > 0 {
		return fmt.Errorf("%d of %d devices failed", sum.Failed, sum.Total)
	}
	if sum.PushError != "" {
		return errors.New("push failed: " + sum.PushError)
	}
	return nil
}

// serve runs the scheduler and HTTP server until signalled to stop. SIGHUP
// reloads the configuration in place.
func serve(cfg *config.Config, r *runner.Runner, m *metrics.Metrics, log *slog.Logger, configPath string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				reload(r, m, log, configPath)
			}
		}
	}()

	go r.Serve(ctx)

	srv := httpapi.New(httpapi.Config{
		MetricsAddr:    cfg.Listen,
		MetricsPath:    cfg.MetricsPath,
		ManagementAddr: cfg.ManagementAddr(),
	}, r, m, log)
	if err := srv.Serve(ctx); err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

func reload(r *runner.Runner, m *metrics.Metrics, log *slog.Logger, configPath string) {
	log.Info("reloading configuration", "path", configPath)

	newCfg, err := config.Load(configPath)
	if err != nil {
		m.ConfigLoad.WithLabelValues(configPath).Set(0)
		log.Error("reload failed, keeping previous configuration", "err", err)
		return
	}

	// Repository settings only take effect through a fresh handle.
	if old := r.Config(); !reflect.DeepEqual(old.Repo, newCfg.Repo) {
		st, err := store.Open(context.Background(), newCfg.Repo)
		m.ObserveGit(metrics.OpOpen, err)
		if err != nil {
			m.ConfigLoad.WithLabelValues(configPath).Set(0)
			log.Error("reload failed, keeping previous repository", "err", err)
			return
		}
		r.SetStore(st)
		log.Info("repository reopened", "path", newCfg.Repo.Path, "branch", newCfg.Repo.Branch)
	}

	r.Reload(newCfg)
	log.Info("reload complete", "devices", len(newCfg.Devices))
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(cfg.LogFormat, "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func printSummary(sum *runner.RunSummary) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tRESULT\tCHANGED\tDURATION\tDETAIL")
	for _, d := range sum.Devices {
		result := "ok"
		detail := ""
		if !d.OK {
			result = "FAILED"
			detail = fmt.Sprintf("[%s] %s", d.Stage, d.Error)
		} else if d.Changed {
			detail = "commit " + d.Commit[:min(8, len(d.Commit))]
		}
		fmt.Fprintf(tw, "%s\t%s\t%v\t%s\t%s\n",
			d.Device, result, d.Changed, d.Duration.Round(time.Millisecond), detail)
	}
	_ = tw.Flush()

	fmt.Printf("\n%d devices, %d ok, %d failed, %d changed, %s\n",
		sum.Total, sum.Succeeded, sum.Failed, sum.Changed, sum.Duration.Round(time.Millisecond))
	if sum.PushError != "" {
		fmt.Printf("push failed: %s\n", sum.PushError)
	}
}

// writeMetricsFile renders the registry in the Prometheus text format, for
// deployments that scrape with the node_exporter textfile collector instead of
// running jconfig as a daemon.
func writeMetricsFile(reg *prometheus.Registry, path string) error {
	families, err := reg.Gather()
	if err != nil {
		return fmt.Errorf("gather metrics: %w", err)
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := expfmt.NewEncoder(f, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range families {
		if err := enc.Encode(mf); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("encode metrics: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

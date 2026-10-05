package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/banner"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/config"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/handlers"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/metrics"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/middleware"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/pitcher"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/scheduler"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/secrets"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/store"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const usage = `Usage: homerun2-schedule-pitcher [command] [flags]

Commands:
  serve   run the scheduler and the HTTP API (default)
  run     run the checks once and exit (no state, for CI or a ScheduledRun)

Run "homerun2-schedule-pitcher <command> -h" for the flags of a command.
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && (args[0] == "serve" || args[0] == "run") {
		cmd, args = args[0], args[1:]
	} else if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Print(usage)
		return
	}

	cfg := config.Load()
	var err error
	switch cmd {
	case "run":
		err = runOnce(cfg, args)
	default:
		err = serve(cfg, args)
	}
	if err != nil {
		slog.Error(cmd+" failed", "error", err)
		os.Exit(1)
	}
}

func serve(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.StringVar(&cfg.ProfilePath, "profile", cfg.ProfilePath, "path to the SchedulePitcherProfile (env PROFILE_PATH)")
	_ = fs.Parse(args)

	banner.Show()
	config.SetupLogging()
	slog.Info("starting homerun2-schedule-pitcher",
		"version", version, "commit", commit, "date", date, "go", runtime.Version())

	prof, err := profile.Load(cfg.ProfilePath)
	if err != nil {
		return err
	}
	slog.Info("profile loaded", "name", prof.Metadata.Name, "checks", len(prof.Spec.Checks), "timezone", prof.Spec.Defaults.Timezone)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	resolver := secrets.NewResolver(nil)
	pt, readyCheck, err := buildPitcher(ctx, cfg, prof, resolver)
	if err != nil {
		return err
	}
	if prof.Spec.Redis.Addr != "" {
		slog.Warn("spec.redis is not used yet, state is kept in memory", "addr", prof.Spec.Redis.Addr)
	}
	sched, err := scheduler.New(prof, store.NewMemory(), pt, resolver)
	if err != nil {
		return err
	}

	var ready atomic.Bool
	buildInfo := handlers.BuildInfo{Version: version, Commit: commit, Date: date}
	auth := middleware.TokenAuthMiddleware

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handlers.NewHealthHandler(buildInfo))
	mux.HandleFunc("GET /ready", handlers.NewReadyHandler(&ready))
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /api/checks", auth(handlers.NewChecksHandler(sched)))
	mux.HandleFunc("POST /api/checks/{id}/run", auth(handlers.NewRunHandler(sched)))

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           middleware.RequestLogging(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	srvErr := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	if readyCheck != nil {
		waitForPitcher(ctx, readyCheck)
	}
	sched.Start(ctx)
	ready.Store(true)
	slog.Info("scheduler started")

	select {
	case <-ctx.Done():
	case err := <-srvErr:
		return fmt.Errorf("http server: %w", err)
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// waitForPitcher waits until omni-pitcher is ready, so the first runs do not
// fail to deliver. It gives up after two minutes; delivery then retries on
// the next run.
func waitForPitcher(ctx context.Context, ready func(context.Context) error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := ready(ctx)
		if err == nil {
			slog.Info("pitcher is ready")
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			slog.Warn("pitcher is not ready, starting anyway", "error", err)
			return
		}
		slog.Info("waiting for pitcher", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func runOnce(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.StringVar(&cfg.ProfilePath, "profile", cfg.ProfilePath, "path to the SchedulePitcherProfile (env PROFILE_PATH)")
	fs.Bool("once", true, "run the checks once (the only mode of run)")
	dryRun := fs.Bool("dry-run", false, "print the messages instead of pitching them")
	only := fs.String("check", "", "run only the check with this id")
	_ = fs.Parse(args)

	config.SetupLogging()
	prof, err := profile.Load(cfg.ProfilePath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	resolver := secrets.NewResolver(nil)
	var pt pitcher.Pitcher = &pitcher.Writer{W: os.Stdout}
	if !*dryRun {
		if pt, _, err = buildPitcher(ctx, cfg, prof, resolver); err != nil {
			return err
		}
	}
	sched, err := scheduler.New(prof, store.NewMemory(), pt, resolver)
	if err != nil {
		return err
	}

	if *only != "" {
		_, err = sched.Run(ctx, *only)
	} else {
		err = sched.RunAll(ctx)
	}

	statuses, sErr := sched.Statuses(ctx)
	if sErr == nil {
		if *only != "" {
			statuses = slices.DeleteFunc(statuses, func(cs scheduler.CheckStatus) bool { return cs.Check.ID != *only })
		}
		printStatuses(statuses)
	}
	return err
}

func printStatuses(statuses []scheduler.CheckStatus) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHECK\tTYPE\tBAND\tEXPIRY\tRESULT")
	for _, cs := range statuses {
		s := cs.State
		band, result := cs.Band, s.Summary
		if s.Problem != "" {
			result = s.Problem + "; " + s.Summary
		}
		if s.Failing {
			band, result = "could not check", s.LastError
		}
		if cs.Check.Paused {
			band = "paused"
		}
		exp := "-"
		if !s.Expiry.IsZero() {
			exp = s.Expiry.Format("2006-01-02")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", cs.Check.ID, cs.Check.Type, band, exp, result)
	}
	_ = tw.Flush()
}

// buildPitcher returns the delivery target and, for HTTP, a readiness probe.
func buildPitcher(ctx context.Context, cfg config.Config, prof *profile.SchedulePitcherProfile, resolver *secrets.Resolver) (pitcher.Pitcher, func(context.Context) error, error) {
	switch cfg.PitchTarget {
	case "file":
		slog.Info("pitching to file", "path", cfg.PitchFile)
		return &pitcher.File{Path: cfg.PitchFile}, nil, nil
	case "stdout":
		return &pitcher.Writer{W: os.Stdout}, nil, nil
	case "http":
	default:
		return nil, nil, fmt.Errorf("unknown PITCH_TARGET %q (http, file or stdout)", cfg.PitchTarget)
	}

	pc := prof.Spec.Pitcher
	addr := pc.Addr
	if cfg.PitcherAddr != "" {
		addr = cfg.PitcherAddr
	}
	if addr == "" {
		return nil, nil, errors.New("spec.pitcher.addr (or PITCHER_ADDR) is required for PITCH_TARGET=http")
	}
	token := pc.Auth.Token
	switch {
	case cfg.PitcherToken != "":
		token = cfg.PitcherToken
	case pc.Auth.TokenFrom != nil:
		t, err := resolver.Resolve(ctx, pc.Auth.TokenFrom)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving spec.pitcher.auth.tokenFrom: %w", err)
		}
		token = t
	}
	h, err := pitcher.NewHTTP(addr, pc.Format, token, pc.CAFile, pc.Insecure)
	if err != nil {
		return nil, nil, err
	}
	slog.Info("pitching to omni-pitcher", "addr", addr, "format", pc.Format)
	return h, h.Ready, nil
}

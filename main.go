package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"
	// Embedded zone data: minimal images (alpine, distroless) may have no
	// tzdata, and the profile's timezone must load anyway.
	_ "time/tzdata"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/banner"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/config"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/discovery"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/findings"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/handlers"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/metrics"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/middleware"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/pitcher"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/report"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/scheduler"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/secrets"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/store"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/ui"
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
	st, fst, err := buildStore(ctx, cfg, prof, resolver)
	if err != nil {
		return err
	}

	// An agent reports its results to a central instance and pitches nothing
	// itself; the central instance routes its own results through findings.
	agent, err := buildReportSender(ctx, cfg, prof, resolver)
	if err != nil {
		return err
	}
	var pt pitcher.Pitcher = &pitcher.Writer{W: io.Discard}
	var readyCheck func(context.Context) error
	if agent == nil {
		if pt, readyCheck, err = buildPitcher(ctx, cfg, prof, resolver); err != nil {
			return err
		}
	}
	sched, err := scheduler.New(prof, st, pt, resolver)
	if err != nil {
		return err
	}

	var ready atomic.Bool
	buildInfo := handlers.BuildInfo{Version: version, Commit: commit, Date: date}
	auth := middleware.TokenAuthMiddleware

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handlers.NewHealthHandler(buildInfo))
	mux.HandleFunc("GET /ready", handlers.NewReadyHandler(&ready, st.Ping))
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /api/checks", auth(handlers.NewChecksHandler(sched)))
	mux.HandleFunc("POST /api/checks/{id}/run", auth(handlers.NewRunHandler(sched)))
	mux.HandleFunc("GET /api/checks/{id}/history", auth(handlers.NewHistoryHandler(sched)))

	reporter := &report.Reporter{Scheduler: sched, Source: reportSource(prof), System: prof.Spec.Defaults.System}
	var fsvc *findings.Service
	if agent != nil {
		reporter.Sender = agent
		if len(prof.Spec.Reminders) > 0 {
			slog.Warn("spec.reminders is ignored on an agent")
		}
		slog.Info("agent mode: reporting check results", "addr", reportAddr(cfg, prof), "source", reporter.Source)
	} else {
		fc := prof.Spec.Findings
		hb := prof.Spec.Heartbeat
		hbSchedule, err := profile.ParseSchedule(hb.Schedule, prof.Location())
		if err != nil {
			return fmt.Errorf("spec.heartbeat.schedule: %w", err)
		}
		reminders, err := buildReminders(prof)
		if err != nil {
			return err
		}
		hbSources := map[string]time.Duration{}
		for src, d := range hb.Sources {
			hbSources[src] = d.D()
		}
		fsvc = findings.NewService(fst, st, pt, findings.Config{
			Heartbeat: findings.HeartbeatConfig{
				Enabled:    hb.On(),
				Schedule:   hbSchedule,
				StaleAfter: hb.StaleAfter.D(),
				Sources:    hbSources,
				Name:       prof.Metadata.Name,
				Status:     heartbeatStatus(sched),
			},
			Hours:     findings.OfficeHours{Start: *fc.OfficeHours.Start, End: *fc.OfficeHours.End, Loc: prof.Location()},
			AckExpiry: fc.AckExpiry.D(),
			Retention: fc.Retention.D(),
			System:    prof.Spec.Defaults.System,
			Assignee:  prof.Spec.Defaults.Assignee,
			Reminders: reminders,
		})
		mux.HandleFunc("POST /findings", auth(handlers.NewIngestHandler(fsvc)))
		mux.HandleFunc("GET /api/findings", auth(handlers.NewFindingsHandler(fsvc)))
		mux.HandleFunc("POST /api/findings/ack", auth(handlers.NewAckHandler(fsvc)))
		mux.HandleFunc("GET /api/reminders", auth(handlers.NewRemindersHandler(fsvc)))
		mux.HandleFunc("POST /api/reminders/{id}/done", auth(handlers.NewReminderDoneHandler(fsvc)))
		reporter.Sender = report.Local{Service: fsvc}
	}
	sched.DeliverAsFindings(reporter.AfterRun)

	var uiFindings ui.Findings
	mode := "agent"
	if fsvc != nil {
		uiFindings, mode = fsvc, "central"
	}
	webUI := ui.New(sched, uiFindings, ui.NewSessions(cfg.AuthToken), prof.Metadata.Name, version, mode, prof.Location())
	webUI.TokenSecret, webUI.TokenNamespace = cfg.AuthTokenSecret, cfg.PodNamespace
	webUI.Register(mux)
	if cfg.AuthToken == "" {
		slog.Warn("AUTH_TOKEN is not set: the API rejects every request and the UI login is disabled")
	}

	disc, err := buildDiscoverer(prof)
	if err != nil {
		return err
	}
	if disc != nil {
		// The first scan runs before the scheduler starts, so discovered
		// checks are part of the first complete report.
		disc.Once(ctx, sched, sched.Checks)
	}

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
	if disc != nil {
		go func() {
			interval := prof.Spec.Discovery.Interval.D()
			<-time.After(interval)
			disc.Loop(ctx, sched, sched.Checks, interval)
		}()
	}
	if fsvc != nil {
		fsvc.Start(ctx)
	}
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
	var agent report.Sender
	switch {
	case reportAddr(cfg, prof) != "" && *dryRun:
		agent = jsonPrinter{}
	case reportAddr(cfg, prof) != "":
		if agent, err = buildReportSender(ctx, cfg, prof, resolver); err != nil {
			return err
		}
	}
	var pt pitcher.Pitcher = &pitcher.Writer{W: os.Stdout}
	if !*dryRun && agent == nil {
		if pt, _, err = buildPitcher(ctx, cfg, prof, resolver); err != nil {
			return err
		}
	}
	sched, err := scheduler.New(prof, store.NewMemory(), pt, resolver)
	if err != nil {
		return err
	}
	disc, err := buildDiscoverer(prof)
	if err != nil {
		return err
	}
	if disc != nil {
		disc.Once(ctx, sched, sched.Checks)
	}
	if agent != nil {
		// Reported once below, as one complete set.
		sched.DeliverAsFindings(nil)
		if *only != "" {
			return errors.New("--check cannot be combined with spec.report: a report is always the complete set")
		}
	}

	if *only != "" {
		_, err = sched.Run(ctx, *only)
	} else {
		err = sched.RunAll(ctx)
	}

	if agent != nil {
		r := &report.Reporter{Scheduler: sched, Sender: agent, Source: reportSource(prof), System: prof.Spec.Defaults.System}
		err = errors.Join(err, r.Report(ctx))
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
			result = strings.TrimSuffix(s.Problem+"; "+s.Summary, "; ")
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

// buildStore returns the Redis store when an address is configured (profile
// or REDIS_ADDR), otherwise the in-memory store.
// buildReminders turns spec.reminders into reminders of the findings
// service; the profile is validated already.
func buildReminders(prof *profile.SchedulePitcherProfile) ([]findings.Reminder, error) {
	var out []findings.Reminder
	for _, r := range prof.Spec.Reminders {
		fr := findings.Reminder{ID: r.ID, Title: r.Title, Message: r.Message, URL: r.URL, Tags: r.Tags, Recurrence: r.Recurrence}
		for _, l := range r.LeadTimes {
			fr.Lead = max(fr.Lead, l.D())
		}
		var err error
		if r.Recurrence != "" {
			fr.Schedule, err = profile.ParseSchedule(r.Recurrence, prof.Location())
		} else {
			fr.Due, err = profile.ParseDue(r.Due, prof.Location())
		}
		if err != nil {
			return nil, fmt.Errorf("spec.reminders %s: %w", r.ID, err)
		}
		out = append(out, fr)
	}
	return out, nil
}

func buildStore(ctx context.Context, cfg config.Config, prof *profile.SchedulePitcherProfile, resolver *secrets.Resolver) (store.Store, findings.Store, error) {
	rc := prof.Spec.Redis
	addr, port, password := rc.Addr, rc.Port, rc.Password
	if cfg.RedisAddr != "" {
		addr = cfg.RedisAddr
	}
	if cfg.RedisPort != "" {
		port = cfg.RedisPort
	}
	if port == "" {
		port = "6379"
	}
	if addr == "" {
		slog.Warn("no Redis configured, state is kept in memory and lost on restart")
		return store.NewMemory(), findings.NewMemory(), nil
	}
	switch {
	case cfg.RedisPassword != "":
		password = cfg.RedisPassword
	case rc.PasswordFrom != nil:
		p, err := resolver.Resolve(ctx, rc.PasswordFrom)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving spec.redis.passwordFrom: %w", err)
		}
		password = p
	}
	client := redis.NewClient(&redis.Options{Addr: net.JoinHostPort(addr, port), Password: password})
	st := store.NewRedis(client, rc.Prefix)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := st.Ping(pingCtx); err != nil {
		// Not fatal: /ready reports it and runs fail until Redis is back.
		slog.Error("redis is not reachable", "addr", addr, "port", port, "error", err)
	} else {
		slog.Info("state store: redis", "addr", addr, "port", port, "prefix", st.Prefix())
	}
	return st, findings.NewRedis(client, st.Prefix()), nil
}

// heartbeatStatus summarises the checks of this instance for the heartbeat.
func heartbeatStatus(sched *scheduler.Scheduler) func(context.Context) []string {
	return func(ctx context.Context) []string {
		sts, err := sched.Statuses(ctx)
		if err != nil {
			return []string{"Checks: unknown (" + err.Error() + ")"}
		}
		bad, failing := 0, 0
		for _, cs := range sts {
			if cs.State.Failing {
				failing++
			} else if cs.State.Band > 0 {
				bad++
			}
		}
		line := fmt.Sprintf("Checks: %d, not ok: %d", len(sts), bad)
		if failing > 0 {
			line += fmt.Sprintf(", could not check: %d", failing)
		}
		return []string{"Version " + version, line}
	}
}

// buildDiscoverer returns nil when discovery is off.
func buildDiscoverer(prof *profile.SchedulePitcherProfile) (*discovery.Discoverer, error) {
	if !prof.Spec.Discovery.Enabled {
		return nil, nil
	}
	client, err := secrets.NewKubeClient()
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	slog.Info("discovery enabled", "namespaces", prof.Spec.Discovery.Namespaces, "selector", prof.Spec.Discovery.LabelSelector, "interval", prof.Spec.Discovery.Interval.String())
	return &discovery.Discoverer{Client: client, Config: prof.Spec.Discovery, Profile: prof}, nil
}

func reportSource(prof *profile.SchedulePitcherProfile) string {
	if src := prof.Spec.Report.Source; src != "" {
		return src
	}
	return "checks"
}

func reportAddr(cfg config.Config, prof *profile.SchedulePitcherProfile) string {
	if cfg.ReportAddr != "" {
		return cfg.ReportAddr
	}
	return prof.Spec.Report.Addr
}

// buildReportSender returns the sender to a central instance, or nil when
// this instance is not an agent.
func buildReportSender(ctx context.Context, cfg config.Config, prof *profile.SchedulePitcherProfile, resolver *secrets.Resolver) (report.Sender, error) {
	rc := prof.Spec.Report
	addr := reportAddr(cfg, prof)
	if addr == "" {
		return nil, nil
	}
	token := rc.Auth.Token
	switch {
	case cfg.ReportToken != "":
		token = cfg.ReportToken
	case rc.Auth.TokenFrom != nil:
		t, err := resolver.Resolve(ctx, rc.Auth.TokenFrom)
		if err != nil {
			return nil, fmt.Errorf("resolving spec.report.auth.tokenFrom: %w", err)
		}
		token = t
	}
	client, err := pitcher.NewHTTPClient(rc.CAFile, rc.Insecure)
	if err != nil {
		return nil, err
	}
	return report.HTTP{Addr: addr, Token: token, Client: client}, nil
}

// jsonPrinter prints a report instead of sending it (run --dry-run).
type jsonPrinter struct{}

func (jsonPrinter) Send(_ context.Context, r findings.Report) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
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

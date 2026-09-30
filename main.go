package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/banner"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/config"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/handlers"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/middleware"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/pitcher"

	homerun "github.com/stuttgart-things/homerun-library/v3"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	banner.Show()
	config.SetupLogging()

	slog.Info("starting homerun2-schedule-pitcher",
		"version", version,
		"commit", commit,
		"date", date,
		"go", runtime.Version(),
	)

	port := homerun.GetEnv("PORT", "8080")
	mode := homerun.GetEnv("PITCHER_MODE", "redis")

	var p pitcher.Pitcher
	switch mode {
	case "file":
		filePath := homerun.GetEnv("PITCHER_FILE", "pitched.log")
		p = &pitcher.FilePitcher{Path: filePath}
		slog.Info("pitcher mode: file", "path", filePath)
	default:
		redisConfig := config.LoadRedisConfig()
		rp := &pitcher.RedisPitcher{Config: redisConfig}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := rp.HealthCheck(ctx); err != nil {
			slog.Error("redis health check failed", "error", err)
			cancel()
			os.Exit(1)
		}
		cancel()
		p = rp
		slog.Info("pitcher mode: redis", "addr", redisConfig.Addr, "port", redisConfig.Port, "stream", redisConfig.Stream)
	}

	authMiddleware := middleware.TokenAuthMiddleware
	buildInfo := handlers.BuildInfo{Version: version, Commit: commit, Date: date}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handlers.NewHealthHandler(buildInfo))
	mux.HandleFunc("/pitch", authMiddleware(handlers.NewPitchHandler(p)))

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: middleware.RequestLogging(mux),
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down server")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
		os.Exit(1)
	}
	slog.Info("server exited gracefully")

}

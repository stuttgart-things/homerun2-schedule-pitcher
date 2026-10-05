package config

import (
	"log/slog"
	"os"
	"strings"

	homerun "github.com/stuttgart-things/homerun-library/v4"
)

// SetupLogging configures slog as the default logger based on LOG_FORMAT and LOG_LEVEL env vars.
func SetupLogging() {
	format := strings.ToLower(homerun.GetEnv("LOG_FORMAT", "json"))
	levelStr := strings.ToLower(homerun.GetEnv("LOG_LEVEL", "info"))

	var level slog.Level
	switch levelStr {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
}

// Config is the process configuration from environment variables.
type Config struct {
	ProfilePath string
	Port        string
	// PitchTarget is http (omni-pitcher from the profile), file or stdout.
	PitchTarget string
	PitchFile   string
	// PitcherToken overrides spec.pitcher.auth of the profile.
	PitcherToken string
	// PitcherAddr overrides spec.pitcher.addr of the profile.
	PitcherAddr string
}

// Load reads the configuration once at startup.
func Load() Config {
	return Config{
		ProfilePath:  homerun.GetEnv("PROFILE_PATH", "/etc/homerun2-schedule-pitcher/profile.yaml"),
		Port:         homerun.GetEnv("PORT", "8080"),
		PitchTarget:  homerun.GetEnv("PITCH_TARGET", "http"),
		PitchFile:    homerun.GetEnv("PITCH_FILE", "pitched.log"),
		PitcherToken: homerun.GetEnv("PITCHER_TOKEN", ""),
		PitcherAddr:  homerun.GetEnv("PITCHER_ADDR", ""),
	}
}

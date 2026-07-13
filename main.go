package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"credential-gateway/internal/config"
	"credential-gateway/internal/gateway"
)

// version is the build version, baked in by deploy.sh via -ldflags -X.
// A plain `go build` leaves it as "dev".
var version = "dev"

// resolveLogLevel resolves the slog level from the -log-level flag and the
// CG_LOG_LEVEL env var. The flag wins when set; otherwise the env var is used;
// otherwise the level defaults to info. Accepted values (case-insensitive):
// debug, info, warn, error. Any other non-empty value is an error.
func resolveLogLevel(flagVal, envVal string) (slog.Level, error) {
	val := flagVal
	if val == "" {
		val = envVal
	}
	switch strings.ToLower(val) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q: accepted values are debug, info, warn, error", val)
	}
}

func main() {
	configPath := flag.String("config", "", "path to config file (default: search standard locations)")
	validate := flag.Bool("validate", false, "validate config and exit without starting listeners")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	logLevel := flag.String("log-level", "", "log verbosity: debug, info, warn, error (default info; overrides CG_LOG_LEVEL)")
	logSource := flag.Bool("log-source", false, "include source file:line in log lines")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	level, err := resolveLogLevel(*logLevel, os.Getenv("CG_LOG_LEVEL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level, AddSource: *logSource}))
	log.Info("starting", "version", version)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	if *validate {
		log.Info("config is valid")
		os.Exit(0)
	}

	gw := gateway.New(cfg, log)
	if err := gw.Start(); err != nil {
		log.Error("failed to start gateway", "err", err)
		os.Exit(1)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gw.Stop(ctx)
	log.Info("stopped")
}

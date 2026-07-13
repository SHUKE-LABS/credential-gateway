package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"credential-gateway/internal/config"
	"credential-gateway/internal/gateway"
)

// version is the build version, baked in by deploy.sh via -ldflags -X.
// A plain `go build` leaves it as "dev".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to config file (default: search standard locations)")
	validate := flag.Bool("validate", false, "validate config and exit without starting listeners")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
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

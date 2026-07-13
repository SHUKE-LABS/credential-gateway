// Command credential-gateway-admin is a loopback-only web UI for viewing and
// editing the gateway config file. It runs as a dedicated non-root user
// (cg-admin) reaching config.yaml via a POSIX ACL, binds 127.0.0.1 only, and
// cannot restart or reload the gateway. See internal/admin.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"credential-gateway/internal/admin"
)

func main() {
	configPath := flag.String("config", "/etc/credential-gateway/config.yaml", "path to the config file to edit")
	port := flag.Int("port", 8099, "loopback port to bind (host is always 127.0.0.1)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Deliberately no config.Load here: an all-commented seed config is invalid
	// for the gateway, but the admin UI must stay up precisely to let the
	// operator fill it in. We only need the path to exist and be readable.
	if _, err := os.Stat(*configPath); err != nil {
		log.Error("config file not accessible", "path", *configPath, "err", err)
		os.Exit(1)
	}

	srv := admin.New(*configPath, log)
	addr := admin.ListenAddr(*port)
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}

	go func() {
		log.Info("admin UI listening", "addr", addr, "config", *configPath)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("admin server exited", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	log.Info("stopped")
}

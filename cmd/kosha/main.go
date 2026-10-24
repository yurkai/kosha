package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kosha/internal/server"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	cfg, err := parseConfig(os.Args[1:], os.LookupEnv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg server.Config) error {
	app, err := server.New(cfg)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return errors.Join(err, app.Close(context.Background()))
	}
	httpServer := &http.Server{
		Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(ln) }()
	slog.Info("listening", "address", ln.Addr().String())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Stop HTTP acceptance before draining hijacked WebSocket connections.
	return errors.Join(err, httpServer.Shutdown(shutdownCtx), app.Close(shutdownCtx))
}

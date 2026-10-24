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
	cfg := server.DefaultConfig()
	flag.StringVar(&cfg.Address, "addr", cfg.Address, "HTTP listen address")
	flag.StringVar(&cfg.Database, "db", cfg.Database, "persistent SQLite file")
	flag.IntVar(&cfg.MaxRooms, "max-rooms", cfg.MaxRooms, "maximum resident rooms")
	flag.IntVar(&cfg.MaxPeers, "max-peers", cfg.MaxPeers, "maximum participants per room")
	flag.Int64Var(&cfg.MaxMessageBytes, "max-message-bytes", cfg.MaxMessageBytes, "maximum incoming WebSocket message size")
	flag.BoolVar(&cfg.Metrics, "metrics", false, "enable diagnostic /debug/metrics (keep private)")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
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

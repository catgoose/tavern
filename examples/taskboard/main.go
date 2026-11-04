// Command taskboard is a reference Tavern application: a shared task board
// served as HTML with HTMX, kept current over one SSE connection per window.
// See README.md in this directory.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg := DefaultConfig()
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	flag.IntVar(&cfg.Buffer, "buffer", cfg.Buffer, "invalidations queued per connection before it is ended")
	flag.DurationVar(&cfg.Heartbeat, "heartbeat", cfg.Heartbeat, "keepalive interval on idle streams")
	flag.DurationVar(&cfg.WriteTimeout, "write-timeout", cfg.WriteTimeout, "limit for writing one frame to a client")
	flag.IntVar(&cfg.MaxSubscribers, "max-subscribers", cfg.MaxSubscribers, "admission limit on open windows; 0 = unlimited")
	flag.BoolVar(&cfg.TestControls, "test-controls", false, "add the /test/ endpoints the browser tests use; not for real use")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*addr, cfg, logger); err != nil {
		logger.Error("taskboard", "err", err)
		os.Exit(1)
	}
}

func run(addr string, cfg Config, logger *slog.Logger) error {
	app := NewApp(cfg, logger)
	srv := &http.Server{
		Addr:              addr,
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would end every stream at a fixed age.
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("listening", "url", "http://"+addr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	app.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

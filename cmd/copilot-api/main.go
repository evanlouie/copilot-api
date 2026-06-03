package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/httpapi"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "copilot-api:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return serve(args[1:])
		case "purge":
			return purge(args[1:])
		case "help", "--help", "-h":
			usage()
			return nil
		default:
			return fmt.Errorf("unknown command %q", args[0])
		}
	}
	return serve(nil)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "", "HTTP listen address (overrides COPILOT_API_ADDR)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if cfg.APIKey == "" {
		if !isLoopbackListenAddr(cfg.Addr) {
			return fmt.Errorf("COPILOT_API_KEY must be set when binding to non-loopback address %q", cfg.Addr)
		}
		logger.Warn("COPILOT_API_KEY is unset; /v1 endpoints are unauthenticated. Keep the default loopback bind or set COPILOT_API_KEY before exposing the service.")
	}
	if cfg.LogContent {
		logger.Warn("COPILOT_LOG_CONTENT=true: request and response bodies will be logged; this may include prompts, completions, tool arguments, and other sensitive data")
	}
	store := sessionstore.New(cfg.DataDir, cfg.StateDir, cfg.CacheDir)
	if err := store.Ensure(); err != nil {
		return err
	}
	lock, err := sessionstore.AcquireLock(store.LockPath())
	if err != nil {
		return err
	}
	defer lock.Release()

	gw := copilotgw.NewReal(cfg, store, logger)
	startupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := gw.Start(startupCtx); err != nil {
		return err
	}
	defer gw.Stop()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.New(cfg, gw, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr, "data_dir", cfg.DataDir, "state_dir", cfg.StateDir, "cache_dir", cfg.CacheDir)
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return errors.Join(srv.Shutdown(ctx), <-errChIfReady(errCh))
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func errChIfReady(ch <-chan error) <-chan error {
	out := make(chan error, 1)
	go func() {
		select {
		case err := <-ch:
			if errors.Is(err, http.ErrServerClosed) {
				out <- nil
			} else {
				out <- err
			}
		case <-time.After(100 * time.Millisecond):
			out <- nil
		}
	}()
	return out
}

func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func usage() {
	fmt.Println(`Usage:
  copilot-api [serve] [--addr 127.0.0.1:8080]
  copilot-api purge [--dry-run] [--yes]

Environment configuration is documented in README.md.`)
}

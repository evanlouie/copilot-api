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
		case "prune":
			return prune(args[1:])
		case "healthcheck":
			return healthcheck()
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
	if err := cfg.ValidateDirs(); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err := cfg.EnsureConfigDir(); err != nil {
		return err
	}
	lifecycleLock, err := sessionstore.AcquireLock(sessionstore.LifecycleLockPath(cfg.ConfigDir))
	if err != nil {
		return err
	}
	defer func() {
		if err := lifecycleLock.Release(); err != nil {
			logger.Error("failed to release lifecycle lock", "error", err)
		}
	}()
	store := sessionstore.New(cfg.DataDir, cfg.StateDir, cfg.CacheDir)
	store.SetRetentionPolicy(configuredRetentionPolicy(cfg))
	if err := store.Ensure(); err != nil {
		return err
	}
	if cfg.APIKey == "" {
		if !isLoopbackListenAddr(cfg.Addr) {
			return fmt.Errorf("COPILOT_API_KEY must be set when binding to non-loopback address %q", cfg.Addr)
		}
		logger.Warn("COPILOT_API_KEY is unset; /v1 endpoints are unauthenticated. Keep the default loopback bind or set COPILOT_API_KEY before exposing the service.")
	}
	if cfg.LogContent {
		logger.Warn("COPILOT_LOG_CONTENT=true: request and response bodies will be logged; this may include prompts, completions, tool arguments, and other sensitive data")
	}
	lock, err := sessionstore.AcquireLock(store.LockPath())
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			logger.Error("failed to release server lock", "error", err)
		}
	}()
	if _, err := store.Prune(false); err != nil {
		return fmt.Errorf("initial retention prune: %w", err)
	}

	gw := copilotgw.NewReal(cfg, store, logger)
	startupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := gw.Start(startupCtx); err != nil {
		return err
	}
	defer func() {
		if err := gw.Stop(); err != nil {
			logger.Error("failed to stop copilot runtime", "error", err)
		}
	}()
	retentionCtx, stopRetention := context.WithCancel(context.Background())
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		runRetentionLoop(retentionCtx, store, logger, time.Minute)
	}()
	defer func() {
		stopRetention()
		<-retentionDone
	}()

	apiServer := httpapi.New(cfg, gw, logger)
	requestRoot, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		BaseContext: func(net.Listener) context.Context {
			return requestRoot
		},
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
		httpShutdown := make(chan error, 1)
		go func() { httpShutdown <- srv.Shutdown(ctx) }()
		webSocketShutdown := make(chan error, 1)
		go func() { webSocketShutdown <- apiServer.Shutdown(ctx) }()
		cancelRequests()
		webSocketErr := <-webSocketShutdown
		httpErr := <-httpShutdown
		serveErr := <-errCh
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(webSocketErr, httpErr, serveErr)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func runRetentionLoop(ctx context.Context, store *sessionstore.Store, logger *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := store.Prune(false); err != nil {
				logger.Warn("automatic retention prune failed", "error", err)
			}
		}
	}
}

func healthcheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %s", response.Status)
	}
	return nil
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
  copilot-api prune [--dry-run]
  copilot-api healthcheck

Environment configuration is documented in README.md.`)
}

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

const (
	// startupTimeout bounds the initial Copilot runtime handshake.
	startupTimeout = 60 * time.Second
	// shutdownGrace is the budget for draining in-flight HTTP requests and
	// WebSocket sessions before their contexts are cancelled.
	shutdownGrace = 20 * time.Second
	// retentionShutdownWait bounds how long shutdown blocks on a retention prune
	// that is already running. store.Prune is not cancellable, so on a large store
	// the wait is abandoned rather than held open indefinitely.
	retentionShutdownWait = 5 * time.Second
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
	// Trap signals before acquiring any resource. Startup prunes the store and
	// waits up to startupTimeout for the Copilot runtime; a signal delivered
	// before Notify runs takes Go's default disposition and kills the process
	// without running a single defer, orphaning the Copilot CLI child. The
	// 10s docker stop grace and Kubernetes rolling restarts routinely land in
	// that window. The buffer holds the shutdown trigger plus one escalation.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	// A second registration, used only to abort startup work. The signal package
	// delivers to every registered channel, so a signal that cancels startup is
	// still queued on signals for the shutdown handling below.
	startupSignalCtx, stopStartupSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopStartupSignals()

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
	startupCtx, cancelStartup := context.WithTimeout(startupSignalCtx, startupTimeout)
	startErr := gw.Start(startupCtx)
	// Release the startup deadline as soon as Start returns; the context is unused
	// afterwards and deferring the cancel keeps a 60s timer armed for the whole
	// process lifetime.
	cancelStartup()
	if startErr != nil {
		if startupSignalCtx.Err() != nil {
			logger.Info("shutting down before startup completed", "error", startErr)
			return nil
		}
		return startErr
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
		// store.Prune is not cancellable, so a prune that is already running can
		// hold this well past the shutdown budget. Bound the wait and let the
		// process exit; the loop goroutine dies with it.
		if !awaitClose(retentionDone, retentionShutdownWait) {
			logger.Warn("abandoning in-flight retention prune", "timeout", retentionShutdownWait)
		}
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

	select {
	case sig := <-signals:
		logger.Info("shutting down", "signal", sig.String())
		return shutdownServer(logger, srv, apiServer, errCh, signals, cancelRequests, shutdownGrace)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// webSocketShutdowner drains long-lived WebSocket sessions. *httpapi.Server
// implements it.
type webSocketShutdowner interface {
	Shutdown(ctx context.Context) error
}

type shutdownErrors struct {
	webSocket error
	http      error
}

// shutdownServer drains in-flight work before cancelling request contexts.
//
// Every request context descends from the root that cancelRequests cancels, so
// calling it is equivalent to aborting every streaming response mid-token and
// unwinding every WebSocket handler before it can close cleanly. It is therefore
// the escalation rather than the trigger: both Shutdown calls get the full grace
// period, and requests are only cancelled once they have drained, the budget has
// expired, or a second signal has asked for a hard stop. On the drained path the
// caller's deferred cancelRequests is the backstop.
func shutdownServer(logger *slog.Logger, srv *http.Server, webSockets webSocketShutdowner, serveErrCh <-chan error, signals <-chan os.Signal, cancelRequests context.CancelFunc, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	httpShutdown := make(chan error, 1)
	go func() { httpShutdown <- srv.Shutdown(ctx) }()
	webSocketShutdown := make(chan error, 1)
	go func() { webSocketShutdown <- webSockets.Shutdown(ctx) }()
	drained := make(chan shutdownErrors, 1)
	go func() {
		webSocketErr := <-webSocketShutdown
		httpErr := <-httpShutdown
		drained <- shutdownErrors{webSocket: webSocketErr, http: httpErr}
	}()

	var result shutdownErrors
	select {
	case result = <-drained:
	case sig := <-signals:
		logger.Warn("second signal received; cancelling in-flight requests", "signal", sig.String())
		cancelRequests()
		if err := srv.Close(); err != nil {
			logger.Error("failed to close http server", "error", err)
		}
		result = <-drained
	case <-ctx.Done():
		logger.Warn("shutdown grace period expired; cancelling in-flight requests", "grace", grace)
		cancelRequests()
		result = <-drained
	}
	serveErr := <-serveErrCh
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(result.webSocket, result.http, serveErr)
}

// awaitClose waits for done to be closed, giving up after timeout. It reports
// whether done closed within the budget.
func awaitClose(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
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

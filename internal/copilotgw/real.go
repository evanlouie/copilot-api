package copilotgw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/sessionfs"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

type RealGateway struct {
	cfg    config.Config
	log    *slog.Logger
	client *copilot.Client
	fs     *sessionfs.Manager
	store  *sessionstore.Store
	broker *toolproxy.Broker

	modelCache     *modelCache
	modelCacheOnce sync.Once
	// modelsFetcher overrides the upstream model fetch. It is nil in production
	// (the SDK client is used) and set by tests to observe refresh behavior.
	modelsFetcher func(context.Context) ([]Model, error)
	pending       *pendingRunnerRegistry
	active        *activeRunnerRegistry
}

func NewReal(cfg config.Config, store *sessionstore.Store, log *slog.Logger) *RealGateway {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	fs := sessionfs.NewManager(cfg.DataDir)
	opts := newRealClientOptions(cfg)
	return &RealGateway{cfg: cfg, log: log, client: copilot.NewClient(opts), fs: fs, store: store, broker: toolproxy.NewBroker(cfg.ToolCallTTL), modelCache: newModelCache(cfg.ModelsCacheTTL), pending: newPendingRunnerRegistry(), active: newActiveRunnerRegistry()}
}
func newRealClientOptions(cfg config.Config) *copilot.ClientOptions {
	return &copilot.ClientOptions{
		Connection:       copilot.StdioConnection{Path: cfg.CLIPath},
		WorkingDirectory: cfg.StateDir,
		BaseDirectory:    cfg.ConfigDir,
		LogLevel:         "error",
		GitHubToken:      cfg.GitHubToken,
		Mode:             copilot.ModeEmpty,
		SessionFS: &copilot.SessionFSConfig{
			InitialWorkingDirectory: "/",
			SessionStatePath:        sessionfs.SessionStatePath,
			Conventions:             rpc.SessionFSSetProviderConventionsPosix,
		},
	}
}
func (g *RealGateway) Start(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(g.cfg.DataDir, "sessions"), 0o700); err != nil {
		return err
	}
	if err := g.client.Start(ctx); err != nil {
		return err
	}
	status, err := g.client.GetStatus(ctx)
	if err != nil {
		return errors.Join(fmt.Errorf("get copilot runtime status: %w", err), g.client.Stop())
	}
	if g.log != nil {
		g.log.Info("copilot runtime ready", "version", status.Version, "protocol_version", status.ProtocolVersion)
	}
	_, err = g.ListModels(ctx)
	if err != nil {
		return errors.Join(err, g.client.Stop())
	}
	return nil
}
func (g *RealGateway) Stop() error {
	active := g.active.closeAndSnapshot()
	pending := g.pending.drain()
	runners := make([]*turnRunner, 0, len(active)+len(pending))
	seen := map[*turnRunner]struct{}{}
	for _, runner := range append(active, pending...) {
		if runner == nil {
			continue
		}
		if _, exists := seen[runner]; exists {
			continue
		}
		seen[runner] = struct{}{}
		runners = append(runners, runner)
	}
	g.broker.CancelAll(context.Canceled)
	for _, runner := range runners {
		runner.abort()
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var waitErr error
	for _, runner := range runners {
		select {
		case <-runner.closed:
		case <-deadline.C:
			waitErr = fmt.Errorf("timed out waiting for pending turns to stop")
			// Stop the SDK before releasing retention pins so no late callback can
			// save or prune state after its protection has been removed.
			stopErr := g.client.Stop()
			// Do not force-release pins for a runner that has not closed. Its loop
			// remains the sole owner and will release them if it exits; otherwise the
			// process is shutting down and retaining state is safer than pruning it.
			return errors.Join(waitErr, stopErr, g.store.TakeMaintenanceError())
		}
	}
	return errors.Join(waitErr, g.client.Stop(), g.store.TakeMaintenanceError())
}
func (g *RealGateway) Ready(ctx context.Context) error {
	if g.client.RPC == nil {
		return fmt.Errorf("copilot client is not connected")
	}
	_, err := g.ListModels(ctx)
	return errors.Join(err, g.store.TakeMaintenanceError())
}

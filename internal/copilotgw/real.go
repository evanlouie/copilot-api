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
	// sessionOpener overrides how SDK sessions are opened. It is nil in
	// production (the SDK client is used) and set by tests so the gateway's turn
	// machinery can run without a live Copilot CLI subprocess.
	sessionOpener sdkSessionOpener
	pending       *pendingRunnerRegistry
	active        *activeRunnerRegistry
	// warm tracks warm Responses sessions, which own a live SDK session and
	// retention pins but have no runner for active to see.
	warm *warmSessionRegistry
}

func NewReal(cfg config.Config, store *sessionstore.Store, log *slog.Logger) *RealGateway {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	fs := sessionfs.NewManager(cfg.DataDir)
	opts := newRealClientOptions(cfg)
	return &RealGateway{cfg: cfg, log: log, client: copilot.NewClient(opts), fs: fs, store: store, broker: toolproxy.NewBroker(cfg.ToolCallTTL), modelCache: newModelCache(cfg.ModelsCacheTTL), pending: newPendingRunnerRegistry(), active: newActiveRunnerRegistry(), warm: newWarmSessionRegistry()}
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

// trackWarmSession hands a warm Responses session to the gateway's shutdown
// accounting and reports whether the gateway accepted ownership. A false result
// means Stop has already snapshotted the registry, so the caller must tear the
// session down itself instead of handing it to a client.
func (g *RealGateway) trackWarmSession(w *WarmResponseSession) bool {
	if g == nil || w == nil {
		return false
	}
	w.attachRegistry(g.warm)
	return g.warm.add(w)
}

func (g *RealGateway) Stop() error {
	active := g.active.closeAndSnapshot()
	pending := g.pending.drain()
	// Warm sessions have no runner to abort and await; disconnecting each one
	// releases its retention pins and drops its SDK session before the client is
	// stopped below.
	for _, warm := range g.warm.closeAndSnapshot() {
		warm.Disconnect()
	}
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
			return errors.Join(waitErr, stopErr, g.fs.Close(), g.store.TakeMaintenanceError())
		}
	}
	return errors.Join(waitErr, g.client.Stop(), g.fs.Close(), g.store.TakeMaintenanceError())
}
func (g *RealGateway) Ready(ctx context.Context) error {
	if g.client.RPC == nil {
		return fmt.Errorf("copilot client is not connected")
	}
	_, err := g.ListModels(ctx)
	return errors.Join(err, g.store.TakeMaintenanceError())
}

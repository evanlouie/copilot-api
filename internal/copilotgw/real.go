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

	modelsMu          sync.Mutex
	models            []Model
	modelsFetched     time.Time
	modelsCacheTTL    time.Duration
	modelsRefreshing  bool
	modelsRefreshDone chan struct{}
	// modelsFetcher overrides the upstream model fetch. It is nil in production
	// (the SDK client is used) and set by tests to observe refresh behavior.
	modelsFetcher  func(context.Context) ([]Model, error)
	pendingMu      sync.Mutex
	pendingRunners map[string]*turnRunner
}

func NewReal(cfg config.Config, store *sessionstore.Store, log *slog.Logger) *RealGateway {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	fs := sessionfs.NewManager(cfg.DataDir)
	opts := newRealClientOptions(cfg)
	return &RealGateway{cfg: cfg, log: log, client: copilot.NewClient(opts), fs: fs, store: store, broker: toolproxy.NewBroker(cfg.ToolCallTTL), modelsCacheTTL: cfg.ModelsCacheTTL, pendingRunners: map[string]*turnRunner{}}
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
	if err := g.store.Ensure(); err != nil {
		return err
	}
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
func (g *RealGateway) Stop() error { return g.client.Stop() }
func (g *RealGateway) Ready(ctx context.Context) error {
	if g.client.RPC == nil {
		return fmt.Errorf("copilot client is not connected")
	}
	_, err := g.ListModels(ctx)
	return err
}

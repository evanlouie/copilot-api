package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/observability"
	"github.com/evanlouie/copilot-api/internal/openai"
)

type Server struct {
	cfg          config.Config
	gw           copilotgw.HTTPGateway
	log          *slog.Logger
	mux          *http.ServeMux
	webSocketMu  sync.Mutex
	webSockets   map[*websocket.Conn]func()
	webSocketWG  sync.WaitGroup
	shuttingDown bool
	authFailures *failureLogSampler
}

func New(cfg config.Config, gw copilotgw.HTTPGateway, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, gw: gw, log: log, mux: http.NewServeMux(), webSockets: map[*websocket.Conn]func(){}, authFailures: newFailureLogSampler(time.Minute)}
	s.routes()
	return s
}
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = requestLoggingMiddleware(s.log, s.cfg.LogContent, h)
	h = s.authMiddleware(h)
	h = recoverMiddleware(s.log, h)
	h = observability.RequestIDMiddleware(h)
	return h
}

func (s *Server) registerWebSocket(conn *websocket.Conn, shutdown func()) bool {
	s.webSocketMu.Lock()
	if s.shuttingDown {
		s.webSocketMu.Unlock()
		shutdown()
		return false
	}
	s.webSockets[conn] = shutdown
	s.webSocketWG.Add(1)
	s.webSocketMu.Unlock()
	return true
}

func (s *Server) unregisterWebSocket(conn *websocket.Conn) {
	s.webSocketMu.Lock()
	if _, ok := s.webSockets[conn]; ok {
		delete(s.webSockets, conn)
		s.webSocketWG.Done()
	}
	s.webSocketMu.Unlock()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.webSocketMu.Lock()
	s.shuttingDown = true
	connections := make([]*websocket.Conn, 0, len(s.webSockets))
	shutdowns := make([]func(), 0, len(s.webSockets))
	for conn, shutdown := range s.webSockets {
		shutdowns = append(shutdowns, shutdown)
		connections = append(connections, conn)
	}
	s.webSocketMu.Unlock()
	for _, shutdown := range shutdowns {
		go shutdown()
	}
	done := make(chan struct{})
	go func() {
		s.webSocketWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		for _, conn := range connections {
			_ = conn.CloseNow()
		}
		return ctx.Err()
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.ready)
	s.mux.HandleFunc("GET /v1/models", s.models)
	s.mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	s.mux.HandleFunc("POST /v1/responses", s.responses)
	s.mux.HandleFunc("GET /v1/responses", s.responsesWebSocket)
	s.mux.HandleFunc("GET /v1/responses/", s.getResponse)
	s.mux.HandleFunc("DELETE /v1/responses/", s.deleteResponse)
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339Nano)})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.gw.Ready(r.Context()); err != nil {
		if s.log != nil {
			s.log.WarnContext(r.Context(), "readiness check failed", "error", err)
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	models, err := s.gw.ListModels(r.Context())
	if err != nil {
		openai.WriteError(w, openai.Upstream(err.Error()))
		return
	}
	out := openai.ModelList{Object: openai.ObjectList, Data: openAIModels(models, openai.UnixNow())}
	writeJSON(w, http.StatusOK, out)
}

func openAIModels(models []copilotgw.Model, created int64) []openai.Model {
	out := make([]openai.Model, 0, len(models))
	seenIDs := make(map[string]struct{}, len(models))
	appendModel := func(id string, metadata map[string]any) {
		if _, exists := seenIDs[id]; exists {
			return
		}
		seenIDs[id] = struct{}{}
		out = append(out, openai.Model{ID: id, Object: openai.ObjectModel, Created: created, OwnedBy: "github-copilot", Meta: metadata})
	}

	// Preserve the existing canonical catalog as the list prefix so positional
	// consumers do not start selecting aliases after this expansion.
	for _, model := range models {
		appendModel(model.ID, model.Metadata)
	}
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		seenEfforts := make(map[string]struct{}, len(model.SupportedReasoningEfforts)+1)
		efforts := append(append([]string(nil), model.SupportedReasoningEfforts...), model.DefaultReasoningEffort)
		for _, rawEffort := range efforts {
			effort := openai.NormalizeReasoningEffort(rawEffort)
			if effort == "" {
				continue
			}
			if _, exists := seenEfforts[effort]; exists {
				continue
			}
			seenEfforts[effort] = struct{}{}
			appendModel(model.ID+":"+effort, modelAliasMetadata(model.Metadata))
		}
	}
	return out
}

func modelAliasMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return nil
	}
	aliasMetadata := make(map[string]any, len(metadata))
	for key, value := range metadata {
		aliasMetadata[key] = value
	}
	// These fields describe effort choices for a canonical model. Keeping them
	// on a fixed-effort alias would suggest recursively suffixed selectors such
	// as model:xhigh:medium and would misstate the alias's default.
	delete(aliasMetadata, "supported_reasoning_efforts")
	delete(aliasMetadata, "default_reasoning_effort")
	return aliasMetadata
}

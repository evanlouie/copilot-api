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
	out := openai.ModelList{Object: openai.ObjectList}
	now := openai.UnixNow()
	for _, m := range models {
		out.Data = append(out.Data, openai.Model{ID: m.ID, Object: openai.ObjectModel, Created: now, OwnedBy: "github-copilot", Meta: m.Metadata})
	}
	writeJSON(w, http.StatusOK, out)
}

package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/observability"
	"github.com/evanlouie/copilot-api/internal/openai"
)

type Server struct {
	cfg config.Config
	gw  copilotgw.Gateway
	log *slog.Logger
	mux *http.ServeMux
}

func New(cfg config.Config, gw copilotgw.Gateway, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, gw: gw, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = s.authMiddleware(h)
	h = recoverMiddleware(s.log, h)
	h = requestLoggingMiddleware(s.log, s.cfg.LogContent, h)
	h = observability.RequestIDMiddleware(h)
	return h
}
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.ready)
	s.mux.HandleFunc("GET /v1/models", s.models)
	s.mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	s.mux.HandleFunc("POST /v1/responses", s.responses)
	s.mux.HandleFunc("GET /v1/responses/", s.getResponse)
	s.mux.HandleFunc("DELETE /v1/responses/", s.deleteResponse)
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339Nano)})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.gw.Ready(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "error": err.Error()})
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

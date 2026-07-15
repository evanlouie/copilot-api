package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

const webSocketWriteTimeout = 30 * time.Second

type webSocketJSONWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (w *webSocketJSONWriter) WriteResponseEvent(ev openai.ResponseStreamEvent) error {
	return w.write(ev)
}

func (w *webSocketJSONWriter) write(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), webSocketWriteTimeout)
	defer cancel()
	return wsjson.Write(ctx, w.conn, v)
}

func (w *webSocketJSONWriter) writeError(err error, eventID string) error {
	return w.write(openai.NewWebSocketErrorEvent(err, eventID))
}

type responsesWebSocketState struct {
	mu     sync.Mutex
	active bool
	// lastSeen is the time of the most recent client activity or response
	// completion. It seeds the idle watchdog so an in-flight response does not
	// count as idle and the idle clock restarts once generation finishes.
	lastSeen time.Time
	// latestID mirrors OpenAI's latest-response cache lifecycle for bookkeeping.
	// Continuation still falls back to locally persisted records, including
	// store:false records, because this proxy intentionally retains local debug
	// state on personal machines.
	latestID string
	warm     *copilotgw.WarmResponseSession
	wg       sync.WaitGroup
}

func (s *responsesWebSocketState) markActivity() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// idleFor reports whether the connection has had no client activity for at least
// d while no response is being generated. An in-flight response never counts as
// idle, since the client is legitimately waiting on streamed output.
func (s *responsesWebSocketState) idleFor(d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active || s.lastSeen.IsZero() {
		return false
	}
	return time.Since(s.lastSeen) >= d
}

func (s *responsesWebSocketState) tryStart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return false
	}
	s.active = true
	s.wg.Add(1)
	return true
}

func (s *responsesWebSocketState) finish() {
	s.mu.Lock()
	s.active = false
	s.lastSeen = time.Now()
	s.mu.Unlock()
	s.wg.Done()
}

func (s *responsesWebSocketState) remember(resp *openai.Response) {
	if resp == nil || resp.ID == "" {
		return
	}
	s.mu.Lock()
	s.latestID = resp.ID
	s.mu.Unlock()
}

func (s *responsesWebSocketState) evict(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	if s.latestID == id {
		s.latestID = ""
	}
	s.mu.Unlock()
}

func (s *responsesWebSocketState) replaceWarm(warm *copilotgw.WarmResponseSession) {
	s.mu.Lock()
	old := s.warm
	s.warm = warm
	s.mu.Unlock()
	if old != nil && old != warm {
		old.Disconnect()
	}
}

func (s *responsesWebSocketState) takeWarm(previousResponseID string) *copilotgw.WarmResponseSession {
	if previousResponseID == "" {
		s.replaceWarm(nil)
		return nil
	}
	s.mu.Lock()
	warm := s.warm
	if warm == nil || warm.ResponseID() != previousResponseID {
		s.mu.Unlock()
		if warm != nil {
			s.replaceWarm(nil)
		}
		return nil
	}
	s.warm = nil
	s.mu.Unlock()
	return warm
}

func (s *responsesWebSocketState) close() {
	s.replaceWarm(nil)
}

func (s *responsesWebSocketState) wait() {
	s.wg.Wait()
}

func (s *Server) responsesWebSocket(w http.ResponseWriter, r *http.Request) {
	if !isWebSocketUpgrade(r) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		openai.WriteError(w, &openai.APIError{Status: http.StatusUpgradeRequired, Message: "websocket upgrade required", Type: "invalid_request_error", Code: "websocket_upgrade_required"})
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	readLimit := s.cfg.MaxRequestBodyBytes
	if readLimit <= 0 {
		readLimit = config.DefaultMaxRequestBodyBytes
	}
	conn.SetReadLimit(readLimit)

	parent := r.Context()
	if s.cfg.WebSocketMaxLifetime > 0 {
		var cancelLifetime context.CancelFunc
		parent, cancelLifetime = context.WithTimeout(parent, s.cfg.WebSocketMaxLifetime)
		defer cancelLifetime()
	}
	connCtx, cancel := context.WithCancel(parent)
	writer := &webSocketJSONWriter{conn: conn}
	state := &responsesWebSocketState{lastSeen: time.Now()}
	var closeOnce sync.Once
	closeWith := func(status websocket.StatusCode, reason string) {
		closeOnce.Do(func() {
			cancel()
			_ = conn.Close(status, reason)
		})
	}
	defer closeWith(websocket.StatusNormalClosure, "")
	if s.cfg.WebSocketPingInterval > 0 {
		go keepResponsesWebSocketAlive(connCtx, conn, s.cfg.WebSocketPingInterval, closeWith)
	}
	// Enforce the idle timeout from a watchdog rather than the read deadline.
	// Cancelling an in-flight websocket read tears down the connection, so a
	// per-read deadline would abort a long response mid-stream. The watchdog only
	// fires when no response is generating and the client has gone quiet.
	if s.cfg.WebSocketIdleTimeout > 0 {
		go watchResponsesWebSocketIdle(connCtx, state, s.cfg.WebSocketIdleTimeout, func() {
			_ = writer.writeError(openai.InvalidRequest("websocket idle timeout", "body"), "")
			closeWith(websocket.StatusGoingAway, "websocket idle timeout")
		})
	}

	for {
		var raw json.RawMessage
		if err := wsjson.Read(connCtx, conn, &raw); err != nil {
			if errors.Is(err, websocket.ErrMessageTooBig) {
				_ = writer.writeError(&openai.APIError{Status: http.StatusRequestEntityTooLarge, Message: "websocket message exceeds maximum request body size", Type: "invalid_request_error", Code: "request_too_large"}, "")
				break
			}
			if websocket.CloseStatus(err) != -1 || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				break
			}
			_ = writer.writeError(openai.InvalidRequest("invalid JSON websocket message: "+err.Error(), "body"), "")
			continue
		}
		state.markActivity()

		var envelope openai.WebSocketClientEvent
		if err := json.Unmarshal(raw, &envelope); err != nil {
			_ = writer.writeError(openai.InvalidRequest("invalid JSON websocket message: "+err.Error(), "body"), "")
			continue
		}
		switch envelope.Type {
		case "response.create":
			if !state.tryStart() {
				_ = writer.writeError(openai.InvalidRequest("only one response.create may be active per WebSocket connection", "type"), envelope.EventID)
				continue
			}
			go func(raw json.RawMessage, eventID string) {
				defer func() {
					if v := recover(); v != nil {
						if s.log != nil {
							s.log.Error("panic in Responses WebSocket response handler", "panic", v, "stack", string(debug.Stack()))
						}
						_ = writer.writeError(openai.Internal("internal server error"), eventID)
						closeWith(websocket.StatusInternalError, "internal server error")
					}
					state.finish()
				}()
				s.handleWebSocketResponseCreate(connCtx, r, writer, state, closeWith, raw)
			}(raw, envelope.EventID)
		case "":
			_ = writer.writeError(openai.InvalidRequest("websocket event type is required", "type"), envelope.EventID)
		default:
			_ = writer.writeError(openai.InvalidRequest("unsupported websocket event type", "type"), envelope.EventID)
		}
	}
	closeWith(websocket.StatusNormalClosure, "")
	state.close()
	state.wait()
}

func watchResponsesWebSocketIdle(ctx context.Context, state *responsesWebSocketState, idle time.Duration, onIdle func()) {
	interval := idle / 2
	if interval <= 0 {
		interval = idle
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if state.idleFor(idle) {
				onIdle()
				return
			}
		}
	}
}

func keepResponsesWebSocketAlive(ctx context.Context, conn *websocket.Conn, interval time.Duration, closeWith func(websocket.StatusCode, string)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, webSocketWriteTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				closeWith(websocket.StatusGoingAway, "websocket ping failed")
				return
			}
		}
	}
}

func (s *Server) handleWebSocketResponseCreate(parent context.Context, r *http.Request, writer *webSocketJSONWriter, state *responsesWebSocketState, closeWith func(websocket.StatusCode, string), raw json.RawMessage) {
	req, eventID, generate, err := decodeWebSocketResponseCreate(raw)
	if err != nil {
		_ = writer.writeError(err, eventID)
		return
	}
	if !generate && (len(req.Input) == 0 || string(req.Input) == "null") {
		req.Input = json.RawMessage(`""`)
	}
	ctx, cancel := requestContext(parent, s.cfg.RequestTimeout)
	defer cancel()
	gwReq, logFields, err := s.prepareResponseRequest(ctx, &req, openai.NewID("resp_"))
	if err != nil {
		_ = writer.writeError(err, eventID)
		return
	}
	if !generate {
		state.replaceWarm(nil)
		s.logGenerationStarted(r, "responses.websocket", req.Model, logFields.reasoningEffort, logFields.resolvedEffort, logFields.resolved, logFields.continuation)
		res, err := s.gw.WarmResponse(ctx, gwReq)
		if err != nil {
			_ = writer.writeError(err, eventID)
			state.evict(gwReq.PreviousResponseID)
			return
		}
		if err := writeWarmResponseEvents(writer, res.Response); err != nil {
			closeWith(websocket.StatusGoingAway, "response stream closed")
			res.WarmSession.Disconnect()
			return
		}
		state.remember(res.Response)
		state.replaceWarm(res.WarmSession)
		return
	}
	gwReq.WarmSession = state.takeWarm(gwReq.PreviousResponseID)
	s.logGenerationStarted(r, "responses.websocket", req.Model, logFields.reasoningEffort, logFields.resolvedEffort, logFields.resolved, logFields.continuation)
	ch, err := s.gw.StreamResponse(ctx, gwReq)
	if err != nil {
		if gwReq.WarmSession != nil {
			gwReq.WarmSession.Disconnect()
		}
		_ = writer.writeError(err, eventID)
		state.evict(gwReq.PreviousResponseID)
		return
	}
	result := writeResponseStreamEvents(ctx, writer, gwReq, ch)
	if result.Err != nil {
		state.evict(gwReq.PreviousResponseID)
		if result.WriteFailed {
			cancel()
			closeWith(websocket.StatusGoingAway, "response stream closed")
			return
		}
		_ = writer.writeError(result.Err, eventID)
		return
	}
	state.remember(result.Response)
}

func decodeWebSocketResponseCreate(raw json.RawMessage) (openai.ResponsesRequest, string, bool, error) {
	var ev openai.ResponseCreateEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return openai.ResponsesRequest{}, "", true, openai.InvalidRequest("invalid response.create event: "+err.Error(), "body")
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return openai.ResponsesRequest{}, ev.EventID, true, openai.InvalidRequest("invalid response.create event: "+err.Error(), "body")
	}
	merged := map[string]json.RawMessage{}
	for name, value := range fields {
		switch name {
		case "type", "event_id", "response":
			continue
		default:
			merged[name] = value
		}
	}
	if len(ev.Response) > 0 && !bytes.Equal(bytes.TrimSpace(ev.Response), []byte("null")) {
		var responseFields map[string]json.RawMessage
		if err := json.Unmarshal(ev.Response, &responseFields); err != nil || responseFields == nil {
			return openai.ResponsesRequest{}, ev.EventID, true, openai.InvalidRequest("response must be an object", "response")
		}
		for name, value := range responseFields {
			merged[name] = value
		}
	}
	generate := true
	if raw, ok := merged["generate"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &generate); err != nil {
			return openai.ResponsesRequest{}, ev.EventID, true, openai.InvalidRequest("generate must be a boolean", "generate")
		}
	}
	delete(merged, "stream")
	delete(merged, "background")
	delete(merged, "generate")
	payload, err := json.Marshal(merged)
	if err != nil {
		return openai.ResponsesRequest{}, ev.EventID, true, openai.Internal("failed to decode response.create event")
	}
	var req openai.ResponsesRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return openai.ResponsesRequest{}, ev.EventID, true, openai.InvalidRequest("invalid response.create request: "+err.Error(), "body")
	}
	if req.Raw == nil {
		req.Raw = map[string]json.RawMessage{}
	}
	return req, ev.EventID, generate, nil
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
			return true
		}
	}
	return false
}

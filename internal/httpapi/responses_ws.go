package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

const webSocketWriteTimeout = 30 * time.Second

type webSocketJSONWriter struct {
	conn *websocket.Conn
	// ctx is the connection's write context, which is deliberately not the
	// context that bounds the connection's upstream work. Every frame is
	// written under it, so a write to a black-holed peer stops as soon as the
	// connection is definitively gone instead of holding mu for the full write
	// timeout past that point - but cancelling the work does not sever a frame
	// mid-write. See responsesWebSocket for why the two must stay distinct.
	ctx context.Context
	mu  sync.Mutex
}

func (w *webSocketJSONWriter) name() string { return "websocket" }

func (w *webSocketJSONWriter) writeResponseEventPayload(_ openai.ResponseStreamEvent, payload []byte) error {
	return w.writePayload(payload)
}

// writeContext bounds a single frame write: the write timeout, but never past
// the life of the connection the frame belongs to.
func (w *webSocketJSONWriter) writeContext() (context.Context, context.CancelFunc) {
	parent := w.ctx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, webSocketWriteTimeout)
}

func (w *webSocketJSONWriter) write(v any) error { return w.writeReleasing(v, nil) }

// releaseThenWrite is the ordering every frame writer here shares: hand a
// connection-level slot back under the write mutex, then put the frame on the
// wire. Releasing there rather than after the write lets a caller free the slot
// at the exact moment the client can first observe the frame, while a frame that
// depends on the released slot still cannot overtake this one.
func (w *webSocketJSONWriter) releaseThenWrite(release func(), write func(context.Context) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if release != nil {
		release()
	}
	ctx, cancel := w.writeContext()
	defer cancel()
	return write(ctx)
}

func (w *webSocketJSONWriter) writeReleasing(v any, release func()) error {
	return w.releaseThenWrite(release, func(ctx context.Context) error {
		return wsjson.Write(ctx, w.conn, v)
	})
}

// writePayload writes already-encoded JSON, which is what wsjson.Write does
// after marshaling. Encoding above the transport lets the WebSocket share the
// SSE stream's event logging.
func (w *webSocketJSONWriter) writePayload(payload []byte) error {
	return w.writePayloadReleasing(payload, nil)
}

func (w *webSocketJSONWriter) writePayloadReleasing(payload []byte, release func()) error {
	return w.releaseThenWrite(release, func(ctx context.Context) error {
		return w.conn.Write(ctx, websocket.MessageText, payload)
	})
}

// framePayloadWriter is the part of the connection writer a response's event
// transport needs: put one encoded frame on the wire, optionally handing a slot
// back under the same lock that orders frames.
type framePayloadWriter interface {
	name() string
	writePayload(payload []byte) error
	writePayloadReleasing(payload []byte, release func()) error
}

// responseSlotTransport writes one response's events and frees the connection's
// single response slot as part of putting that response's terminal frame on the
// wire.
//
// The slot has to be free no later than the moment the client can observe the
// terminal event, because sending the next response.create the instant
// response.completed arrives is the obvious and correct client behaviour.
// Releasing it from the handler goroutine's deferred finish - after the frame is
// already on the wire - answers that client with "only one response.create may
// be active per WebSocket connection", which is a spurious error rather than a
// flaky test.
//
// The release runs under the writer's mutex, so the next response's frames still
// cannot overtake this one even if that client raced ahead.
type responseSlotTransport struct {
	writer  framePayloadWriter
	release func()
}

func (t *responseSlotTransport) name() string { return t.writer.name() }

func (t *responseSlotTransport) writeResponseEventPayload(ev openai.ResponseStreamEvent, payload []byte) error {
	if isTerminalResponseEventType(ev.Type) {
		return t.writer.writePayloadReleasing(payload, t.release)
	}
	return t.writer.writePayload(payload)
}

func (w *webSocketJSONWriter) writeError(err error, eventID string, seq int64) error {
	return w.write(NewWebSocketErrorEvent(err, eventID, seq))
}

// writeErrorReleasing ends a response with an error envelope and frees the
// connection's response slot in the same place responseSlotTransport frees it
// for a terminal event: an error is just as final, and a client is just as
// entitled to retry the instant it sees one.
func (w *webSocketJSONWriter) writeErrorReleasing(err error, eventID string, seq int64, release func()) error {
	return w.writeReleasing(NewWebSocketErrorEvent(err, eventID, seq), release)
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
	// closed records that the connection has been torn down. Once set, warm is
	// permanently nil and replaceWarm refuses rather than stores.
	closed bool
	wg     sync.WaitGroup
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

// endActive frees the connection's single response slot. It is idempotent and
// deliberately separate from finish: the slot has to be free by the time the
// client can observe the response's terminal frame, whereas the wait group must
// stay held until the handler goroutine has actually returned and can no longer
// install anything into the state.
func (s *responsesWebSocketState) endActive() {
	s.mu.Lock()
	s.active = false
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

func (s *responsesWebSocketState) finish() {
	s.endActive()
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

// replaceWarm installs the connection's warm session, disconnecting whatever it
// replaces.
//
// Once the state is closed it refuses instead: the incoming session is
// disconnected and nothing is stored. A response.create still in flight when
// the connection is torn down would otherwise park a live SDK session and its
// two retention pins in state that has already been dropped, and nothing in
// this package would ever disconnect it. responsesWebSocket also waits for
// that work before closing the state, but refusing here is what makes the
// invariant structural rather than a property of one call ordering. It is the
// same refuse-and-disconnect shape copilotgw's warm session registry uses.
func (s *responsesWebSocketState) replaceWarm(warm *copilotgw.WarmResponseSession) {
	s.mu.Lock()
	old := s.warm
	closed := s.closed
	if closed {
		s.warm = nil
	} else {
		s.warm = warm
	}
	s.mu.Unlock()
	if old != nil && old != warm {
		old.Disconnect()
	}
	if closed && warm != nil {
		warm.Disconnect()
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

// shutdown ends the connection's state at the end of responsesWebSocket. It
// waits for in-flight response.create work before dropping what that work can
// still install: the !generate branch ends in replaceWarm, and close is what
// disconnects the session it installs. Closing first left such a session in
// state nobody owned any more.
func (s *responsesWebSocketState) shutdown() {
	s.wait()
	s.close()
}

// close drops the connection's warm session and refuses any later one. Callers
// must wait for in-flight response.create work first: this is what tears down
// the session that work may have installed.
func (s *responsesWebSocketState) close() {
	s.mu.Lock()
	s.closed = true
	old := s.warm
	s.warm = nil
	s.mu.Unlock()
	old.Disconnect()
}

func (s *responsesWebSocketState) wait() {
	s.wg.Wait()
}

func (s *Server) responsesWebSocket(w http.ResponseWriter, r *http.Request) {
	if !isWebSocketUpgrade(r) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		writeErrorObject(w, http.StatusUpgradeRequired, openai.ErrorObject{Message: "websocket upgrade required", Type: "invalid_request_error", Code: "websocket_upgrade_required"})
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	// Always pin the read limit explicitly. coder/websocket applies a 32 KiB
	// message limit unless SetReadLimit is called, which is far below a
	// realistic response.create payload, so leaving the library default in
	// effect implicitly would reject ordinary traffic. -1 is the library's
	// documented "unlimited" sentinel (see (*websocket.Conn).SetReadLimit).
	readLimit := s.cfg.MaxRequestBodyBytes
	if readLimit <= 0 {
		readLimit = -1
	}
	conn.SetReadLimit(readLimit)

	parent := r.Context()
	if s.cfg.WebSocketMaxLifetime > 0 {
		var cancelLifetime context.CancelFunc
		parent, cancelLifetime = context.WithTimeout(parent, s.cfg.WebSocketMaxLifetime)
		defer cancelLifetime()
	}
	// Two contexts, because the connection has two independent lifetimes.
	//
	// connCtx bounds the *work* this connection starts: in-flight responses and
	// the gateway producers behind them, and the ping and idle watchdogs.
	// closeWith cancels it first and synchronously, so upstream Copilot turns
	// stop burning quota immediately rather than after the close handshake's
	// worst case (5s writing the close frame plus 5s waiting for the reply, the
	// latter behind the read mutex this connection's own read loop is holding).
	//
	// writeCtx bounds *frame writes*, and is deliberately neither connCtx nor
	// derived from the request. coder/websocket's writeFrame installs a
	// context.AfterFunc on the write context that calls Conn.close (see
	// setupWriteTimeout in conn.go), exactly as setupReadTimeout does for reads.
	// Writing frames under connCtx therefore means that cancelling work while a
	// frame is in flight closes the socket outright, and the close frame that
	// follows is dropped by writeFrame's
	// `select { case <-c.closed: return net.ErrClosed }` - the client sees EOF
	// rather than a status. writeCtx is cancelled only once conn.Close has
	// returned, which keeps the handshake intact while still guaranteeing that
	// no frame write outlives the connection.
	//
	// The read loop below deliberately does NOT read with connCtx either, for
	// the read-side half of the same hazard. conn.Close is what wakes the read
	// loop; connCtx.Err() is what tells it the teardown was ours.
	connCtx, cancelWork := context.WithCancel(parent)
	writeCtx, cancelWrites := context.WithCancel(context.Background())
	defer cancelWrites()
	writer := &webSocketJSONWriter{conn: conn, ctx: writeCtx}
	state := &responsesWebSocketState{lastSeen: time.Now()}
	var closeOnce sync.Once
	closeWith := func(status websocket.StatusCode, reason string) {
		closeOnce.Do(func() {
			// Order matters in both directions. Cancelling work first is what
			// makes shutdown latency independent of an unresponsive peer.
			// Cancelling writes only after the handshake is what keeps the close
			// frame from being severed by that same cancellation.
			cancelWork()
			_ = conn.Close(status, reason)
			cancelWrites()
		})
	}
	if !s.registerWebSocket(conn, func() { closeWith(websocket.StatusGoingAway, "server shutting down") }) {
		return
	}
	defer s.unregisterWebSocket(conn)
	defer closeWith(websocket.StatusNormalClosure, "")
	if s.cfg.WebSocketPingInterval > 0 {
		go keepResponsesWebSocketAlive(connCtx, writeCtx, conn, s.cfg.WebSocketPingInterval, closeWith)
	}
	// Enforce the idle timeout from a watchdog rather than the read deadline.
	// Cancelling an in-flight websocket read tears down the connection, so a
	// per-read deadline would abort a long response mid-stream. The watchdog only
	// fires when no response is generating and the client has gone quiet.
	if s.cfg.WebSocketIdleTimeout > 0 {
		go watchResponsesWebSocketIdle(connCtx, state, s.cfg.WebSocketIdleTimeout, func() {
			_ = writer.writeError(apierr.InvalidRequest("websocket idle timeout", "body"), "", 0)
			closeWith(websocket.StatusGoingAway, "websocket idle timeout")
		})
	}

	for {
		var raw json.RawMessage
		if err := wsjson.Read(parent, conn, &raw); err != nil {
			if errors.Is(err, websocket.ErrMessageTooBig) {
				// The limit is enforced inside coder/websocket's message
				// reader, and by the time the error surfaces the connection is
				// already unrecoverable: the library has written its own
				// StatusMessageTooBig close frame (so every subsequent data
				// write fails with net.ErrClosed, and a correlated error event
				// can no longer be delivered), and it abandoned the message
				// mid-stream, leaving the reader unfinished and the rest of the
				// payload unread, so the next read fails with "previous message
				// not read to completion". There is also no event_id to
				// correlate against, because the frame was never parsed. Close
				// deliberately with a status and reason naming the limit rather
				// than letting the deferred normal-closure win.
				if s.log != nil {
					s.log.Warn("websocket message exceeded the request body limit", "limit_bytes", readLimit)
				}
				closeWith(websocket.StatusMessageTooBig, fmt.Sprintf("websocket message exceeds the %d byte request body limit", readLimit))
				break
			}
			// connCtx.Err() covers every teardown this server initiated: closeWith
			// cancels before it closes, so a read that fails with the connection
			// already cancelled is our own shutdown surfacing as net.ErrClosed and
			// must end the loop rather than be reported back to a dead peer.
			if websocket.CloseStatus(err) != -1 || connCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				break
			}
			// Anything left is a connection this loop can no longer read from, and
			// which coder/websocket has already closed for us. A transport failure
			// is closed before the error surfaces; a payload that is not JSON at
			// all is closed by wsjson with StatusInvalidFramePayloadData, which is
			// what the WebSocket spec prescribes for non-conforming payload data
			// and is the behaviour this proxy wants (wsjson.Read calls c.Close
			// before returning the unmarshal error). Either way every later write
			// returns net.ErrClosed the instant it is issued, so no error envelope
			// can reach the client and none is attempted: a client that sends
			// malformed JSON gets the 1007 close and nothing else.
			//
			// Ending the loop is the part that matters. Continuing spun this
			// goroutine at full speed forever - it never exited, so the
			// connection's warm session was never disconnected and its retention
			// pins were never released. A client that vanishes without a close
			// handshake, the ordinary outcome of a dropped network or a killed
			// client, is enough to reach it.
			break
		}
		state.markActivity()

		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			_ = writer.writeError(apierr.InvalidRequest("invalid JSON websocket message", "body"), "", 0)
			continue
		}
		eventType, eventID, err := webSocketEnvelopeFields(fields)
		if err != nil {
			_ = writer.writeError(err, eventID, 0)
			continue
		}
		switch eventType {
		case "response.create":
			if !state.tryStart() {
				_ = writer.writeError(apierr.InvalidRequest("only one response.create may be active per WebSocket connection", "type"), eventID, 0)
				continue
			}
			go func(fields map[string]json.RawMessage, eventID string) {
				defer func() {
					if v := recover(); v != nil {
						if s.log != nil {
							s.log.Error("panic in Responses WebSocket response handler", "panic", v, "stack", string(debug.Stack()))
						}
						_ = writer.writeError(apierr.Internal("internal server error"), eventID, 0)
						closeWith(websocket.StatusInternalError, "internal server error")
					}
					state.finish()
				}()
				s.handleWebSocketResponseCreate(connCtx, r, writer, state, closeWith, fields)
			}(fields, eventID)
		case "":
			_ = writer.writeError(apierr.InvalidRequest("websocket event type is required", "type"), eventID, 0)
		default:
			_ = writer.writeError(apierr.InvalidRequest("unsupported websocket event type", "type"), eventID, 0)
		}
	}
	closeWith(websocket.StatusNormalClosure, "")
	state.shutdown()
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

// keepResponsesWebSocketAlive takes both of the connection's contexts: ctx ends
// the loop when the connection's work is cancelled, while writeCtx bounds the
// ping frame itself. A ping is a control frame like any other, so bounding it
// with ctx would let cancellation close the socket mid-ping and sever the close
// frame - the same hazard responsesWebSocket documents for data frames.
func keepResponsesWebSocketAlive(ctx, writeCtx context.Context, conn *websocket.Conn, interval time.Duration, closeWith func(websocket.StatusCode, string)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(writeCtx, webSocketWriteTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				closeWith(websocket.StatusGoingAway, "websocket ping failed")
				return
			}
		}
	}
}

func (s *Server) handleWebSocketResponseCreate(parent context.Context, r *http.Request, writer *webSocketJSONWriter, state *responsesWebSocketState, closeWith func(websocket.StatusCode, string), fields map[string]json.RawMessage) {
	// Every way this response can end - a terminal event, an error envelope, or
	// the deferred finish for the paths that write neither - frees the
	// connection's response slot no later than the frame that tells the client the
	// response is over.
	// The error envelope is one of this response's stream events, so it draws its
	// sequence number from the same encoder the streamed events use rather than
	// restarting a parallel count. Before that encoder exists the response has
	// emitted nothing, so a failure there is event zero either way.
	var events *responseStreamEncoder
	fail := func(err error, eventID string) {
		var seq int64
		if events != nil {
			seq = events.nextSequence()
		}
		_ = writer.writeErrorReleasing(err, eventID, seq, state.endActive)
	}
	req, eventID, generate, err := decodeWebSocketResponseCreateFields(fields)
	if err != nil {
		fail(err, eventID)
		return
	}
	if !generate && (len(req.Input) == 0 || string(req.Input) == "null") {
		req.Input = json.RawMessage(`""`)
	}
	ctx, cancel := requestContext(parent, s.cfg.RequestTimeout)
	defer cancel()
	events = newResponseStreamEncoder(newLoggedResponseEventWriter(s, ctx, &responseSlotTransport{writer: writer, release: state.endActive}))
	gwReq, logFields, err := s.prepareResponseRequest(ctx, &req, openai.NewID("resp_"))
	if err != nil {
		fail(err, eventID)
		return
	}
	if !generate {
		state.replaceWarm(nil)
		s.logGenerationStarted(r, "responses.websocket", req.Model, logFields.reasoningEffort, logFields.resolvedEffort, logFields.resolved, logFields.continuation)
		res, err := s.gw.WarmResponse(ctx, gwReq)
		if err != nil {
			state.evict(gwReq.PreviousResponseID)
			fail(err, eventID)
			return
		}
		// Install before the terminal frame, not after it. Writing that frame is
		// what frees the response slot, so the next response.create - which a
		// client is entitled to send the instant it sees response.completed - has
		// to find this warm session already in place or it would resume the SDK
		// session the slow way and strand this one.
		state.remember(res.Response)
		state.replaceWarm(res.WarmSession)
		if err := writeWarmResponseEvents(events, res.Response); err != nil {
			closeWith(websocket.StatusGoingAway, "response stream closed")
			// Tear down what is still ours. If the terminal frame is the one that
			// failed, the slot is already free and the next response.create may have
			// taken this session; then replaceWarm finds nothing and disconnects
			// nothing, which is right - it belongs to that request now.
			state.replaceWarm(nil)
			return
		}
		return
	}
	gwReq.WarmSession = state.takeWarm(gwReq.PreviousResponseID)
	s.logGenerationStarted(r, "responses.websocket", req.Model, logFields.reasoningEffort, logFields.resolvedEffort, logFields.resolved, logFields.continuation)
	ch, err := s.gw.StreamResponse(ctx, gwReq)
	if err != nil {
		if gwReq.WarmSession != nil {
			gwReq.WarmSession.Disconnect()
		}
		state.evict(gwReq.PreviousResponseID)
		fail(err, eventID)
		return
	}
	result := writeResponseStreamEvents(ctx, events, gwReq, s.cfg.MaxTurnOutputBytes, s.suppressReasoning(), ch)
	if result.Err != nil {
		state.evict(gwReq.PreviousResponseID)
		if result.WriteFailed {
			cancel()
			closeWith(websocket.StatusGoingAway, "response stream closed")
			return
		}
		if !result.FailureWritten {
			fail(result.Err, eventID)
		}
		return
	}
	state.remember(result.Response)
}

func webSocketEnvelopeFields(fields map[string]json.RawMessage) (string, string, error) {
	var eventType string
	if raw, ok := fields["type"]; ok {
		if err := json.Unmarshal(raw, &eventType); err != nil {
			return "", "", apierr.InvalidRequest("websocket event type must be a string", "type")
		}
	}
	var eventID string
	if raw, ok := fields["event_id"]; ok {
		if err := json.Unmarshal(raw, &eventID); err != nil {
			return eventType, "", apierr.InvalidRequest("event_id must be a string", "event_id")
		}
	}
	return eventType, eventID, nil
}

func decodeWebSocketResponseCreateFields(fields map[string]json.RawMessage) (openai.ResponsesRequest, string, bool, error) {
	_, eventID, err := webSocketEnvelopeFields(fields)
	if err != nil {
		return openai.ResponsesRequest{}, eventID, true, err
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
	responseRaw := fields["response"]
	if len(responseRaw) > 0 && !bytes.Equal(bytes.TrimSpace(responseRaw), []byte("null")) {
		var responseFields map[string]json.RawMessage
		if err := json.Unmarshal(responseRaw, &responseFields); err != nil || responseFields == nil {
			return openai.ResponsesRequest{}, eventID, true, apierr.InvalidRequest("response must be an object", "response")
		}
		for name, value := range responseFields {
			merged[name] = value
		}
	}
	generate := true
	if raw, ok := merged["generate"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &generate); err != nil {
			return openai.ResponsesRequest{}, eventID, true, apierr.InvalidRequest("generate must be a boolean", "generate")
		}
	}
	delete(merged, "stream")
	delete(merged, "background")
	delete(merged, "generate")
	req, err := openai.ResponsesRequestFromFields(merged)
	if err != nil {
		return openai.ResponsesRequest{}, eventID, true, apierr.InvalidRequest("invalid response.create request: "+err.Error(), "body")
	}
	return req, eventID, generate, nil
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

package copilotgw

import "sync"

// warmSessionRegistry tracks every warm Responses session the gateway created,
// so gateway shutdown can tear down SDK activity that has no runner behind it.
//
// activeRunnerRegistry only ever sees *turnRunner. A WarmResponseSession has no
// runner at all: it parks a live *copilot.Session, a *toolproxy.RequestTools and
// a set of retention pins with no goroutine of its own, owned entirely by the
// per-connection state in internal/httpapi. Without this registry the gateway
// had zero visibility into an SDK session it had itself created, so Stop neither
// disconnected it nor released its pins.
//
// The lifecycle contract matches activeRunnerRegistry: close so nothing new can
// register, then drain what did.
type warmSessionRegistry struct {
	mu       sync.Mutex
	sessions map[*WarmResponseSession]struct{}
	closed   bool
}

func newWarmSessionRegistry() *warmSessionRegistry {
	return &warmSessionRegistry{sessions: map[*WarmResponseSession]struct{}{}}
}

// add registers a warm session and reports whether the gateway now owns its
// shutdown. It reports false once the registry is closed: Stop has already taken
// its snapshot, so a session added afterwards would never be drained. The caller
// must tear such a session down itself rather than hand it to a client.
func (r *warmSessionRegistry) add(w *WarmResponseSession) bool {
	if r == nil || w == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.sessions[w] = struct{}{}
	return true
}

// remove deregisters a warm session whose ownership has moved elsewhere, either
// to a turnRunner (which activeRunnerRegistry then covers) or to a caller that
// has already torn it down.
func (r *warmSessionRegistry) remove(w *WarmResponseSession) {
	if r == nil || w == nil {
		return
	}
	r.mu.Lock()
	delete(r.sessions, w)
	r.mu.Unlock()
}

// tracked reports whether a warm session is still the gateway's to shut down.
func (r *warmSessionRegistry) tracked(w *WarmResponseSession) bool {
	if r == nil || w == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.sessions[w]
	return ok
}

func (r *warmSessionRegistry) closeAndSnapshot() []*WarmResponseSession {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.closed = true
	out := make([]*WarmResponseSession, 0, len(r.sessions))
	for session := range r.sessions {
		out = append(out, session)
	}
	r.sessions = map[*WarmResponseSession]struct{}{}
	r.mu.Unlock()
	return out
}

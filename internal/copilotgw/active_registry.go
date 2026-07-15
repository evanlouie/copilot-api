package copilotgw

import "sync"

// activeRunnerRegistry tracks every runner, not only runners parked on tool
// calls, so gateway shutdown can abort and await all SDK activity.
type activeRunnerRegistry struct {
	mu      sync.Mutex
	runners map[*turnRunner]struct{}
	closed  bool
}

func newActiveRunnerRegistry() *activeRunnerRegistry {
	return &activeRunnerRegistry{runners: map[*turnRunner]struct{}{}}
}

func (r *activeRunnerRegistry) add(runner *turnRunner) bool {
	if r == nil || runner == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.runners[runner] = struct{}{}
	return true
}

func (r *activeRunnerRegistry) remove(runner *turnRunner) {
	if r == nil || runner == nil {
		return
	}
	r.mu.Lock()
	delete(r.runners, runner)
	r.mu.Unlock()
}

func (r *activeRunnerRegistry) closeAndSnapshot() []*turnRunner {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.closed = true
	out := make([]*turnRunner, 0, len(r.runners))
	for runner := range r.runners {
		out = append(out, runner)
	}
	r.mu.Unlock()
	return out
}

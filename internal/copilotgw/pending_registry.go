package copilotgw

import "sync"

type pendingRunnerRegistry struct {
	mu      sync.Mutex
	runners map[string]*turnRunner
	closed  bool
}

func newPendingRunnerRegistry() *pendingRunnerRegistry {
	return &pendingRunnerRegistry{runners: map[string]*turnRunner{}}
}

func (r *pendingRunnerRegistry) put(batchID string, runner *turnRunner) {
	if r == nil || batchID == "" || runner == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		runner.abort()
		return
	}
	r.runners[batchID] = runner
	r.mu.Unlock()
}

func (r *pendingRunnerRegistry) get(batchID string) *turnRunner {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runners[batchID]
}

func (r *pendingRunnerRegistry) drain() []*turnRunner {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	unique := map[*turnRunner]struct{}{}
	for _, runner := range r.runners {
		unique[runner] = struct{}{}
	}
	r.runners = map[string]*turnRunner{}
	r.closed = true
	r.mu.Unlock()
	out := make([]*turnRunner, 0, len(unique))
	for runner := range unique {
		out = append(out, runner)
	}
	return out
}

func (r *pendingRunnerRegistry) remove(batchID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.runners, batchID)
	r.mu.Unlock()
}

package toolproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// registerLikeTurn replays one N-tool parallel turn's registration pattern:
// CaptureRequests registers the announced set once, then handleInvocation
// registers the same batch again for every tool the SDK invokes.
func registerLikeTurn(broker *Broker, tools int) *Batch {
	batch := newBatch(time.Minute, "resp", "responses", "gpt-5", nil, nil, context.Background())
	for i := range tools {
		batch.ensureCall(fmt.Sprintf("call_%d", i), "lookup", ClientTool{}, json.RawMessage(`{}`), "")
	}
	broker.Register(batch)
	for i := range tools {
		batch.ensureCall(fmt.Sprintf("call_%d", i), "lookup", ClientTool{}, json.RawMessage(`{}`), "")
		broker.Register(batch)
	}
	return batch
}

func expiryHookCount(b *Batch) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.expireHooks)
}

// TestRegisterDoesNotAccumulateExpiryWork pins the cost of registering a batch
// with a broker as O(1) in the number of registrations.
//
// A broker registration is idempotent: the batch either is or is not in the
// broker's maps. Per-registration expiry work is therefore always waste, and
// because each unit of it walks the batch's whole call map at close, it makes
// closing an N-tool turn quadratic in N.
func TestRegisterDoesNotAccumulateExpiryWork(t *testing.T) {
	broker := NewBroker(time.Minute)
	few := registerLikeTurn(broker, 2)
	many := registerLikeTurn(broker, 64)
	defer few.Cancel(context.Canceled)
	defer many.Cancel(context.Canceled)
	if got, want := expiryHookCount(many), expiryHookCount(few); got != want {
		t.Fatalf("a 64-tool turn left %d expiry hooks on its batch and a 2-tool turn left %d; registration must not accumulate per-call expiry work", got, want)
	}
	if got := expiryHookCount(many); got > 1 {
		t.Fatalf("registering one batch 65 times left %d expiry hooks, want at most 1", got)
	}
}

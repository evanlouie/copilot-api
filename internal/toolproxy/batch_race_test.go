package toolproxy

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
)

// TestInvocationDuringExpiryDoesNotRaceOnCallMap drives SDK tool handlers
// against a batch whose TTL expires while the invocations are still in flight.
//
// The handler goroutine checks isOpen, then adds its call under batch.mu, while
// the time.AfterFunc expiry goroutine releases batch.mu inside closeBatch and
// runs the broker's expiry hook, which iterates the same call map. Iterating the
// live map from the broker is unsynchronized against that write: under -race it
// reports a data race, and in production the runtime kills the whole process
// with "fatal error: concurrent map read and map write".
//
// A sub-millisecond TTL plus concurrent handlers places the expiry squarely
// inside the invocation window; the iterations make hitting it reliable.
func TestInvocationDuringExpiryDoesNotRaceOnCallMap(t *testing.T) {
	const (
		iterations = 150
		handlers   = 256
		ttl        = 200 * time.Microsecond
	)
	for i := range iterations {
		broker := NewBroker(ttl)
		rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
		if err != nil {
			t.Fatal(err)
		}
		handler := rt.Tools()[0].Handler
		start := make(chan struct{})
		var wg sync.WaitGroup
		for h := range handlers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				// Every handler blocks until the batch expires or is cancelled,
				// so the returned error is expected and uninteresting here.
				_, _ = handler(copilot.ToolInvocation{
					ToolName:   "lookup",
					ToolCallID: fmt.Sprintf("call_%d_%d", i, h),
					Arguments:  map[string]any{},
				})
			}()
		}
		close(start)
		wg.Wait()
		broker.CancelAll(context.Canceled)
	}
}

// TestRemoveKeepsCallIDsOwnedByAnotherBatch pins the second half of the fix:
// Remove must not evict lookup entries that a later batch re-registered under
// the same call id, which would strand the live batch.
//
// Proxy-minted call ids make a natural collision vanishingly unlikely, so the
// shared id is planted directly on both batches' call maps.
func TestRemoveKeepsCallIDsOwnedByAnotherBatch(t *testing.T) {
	broker := NewBroker(time.Minute)

	stale := newBatch(time.Minute, "", "chat", "gpt-test", nil, nil, context.Background())
	stale.calls["call_shared"] = &Call{OpenAIID: "call_shared", SDKID: "sdk_stale", outCh: make(chan string, 1), errCh: make(chan error, 1)}
	broker.Register(stale)

	live := newBatch(time.Minute, "", "chat", "gpt-test", nil, nil, context.Background())
	live.calls["call_shared"] = &Call{OpenAIID: "call_shared", SDKID: "sdk_live", outCh: make(chan string, 1), errCh: make(chan error, 1)}
	broker.Register(live)

	broker.Remove(stale)

	found, err := broker.FindByCallIDs([]string{"call_shared"})
	if err != nil {
		t.Fatalf("live batch lookup failed after removing the stale batch: %v", err)
	}
	if found.ID != live.ID {
		t.Fatalf("found batch %q, want live batch %q", found.ID, live.ID)
	}
}

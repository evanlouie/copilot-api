package toolproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// benchmarkTurn replays the registration pattern of one N-tool parallel turn.
//
// The SDK announces the whole tool request set once (CaptureRequests) and then
// invokes the tool handler separately for each call (handleInvocation), and
// both paths call Broker.Register on the same batch. Expiry or cancellation
// then closes the batch, which is where any per-registration bookkeeping is
// paid back.
func benchmarkTurn(b *testing.B, broker *Broker, tools int) {
	ctx := context.Background()
	args := json.RawMessage(`{"query":"benchmark"}`)
	sdkIDs := make([]string, tools)
	for i := range sdkIDs {
		sdkIDs[i] = fmt.Sprintf("call_%d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		batch := newBatch(time.Minute, "resp_bench", "responses", "gpt-5", nil, nil, ctx)
		for _, id := range sdkIDs {
			batch.ensureCall(id, "lookup", ClientTool{}, args, "", "")
		}
		// CaptureRequests registers the announced set once.
		broker.Register(batch)
		batch.startTimer()
		// handleInvocation then registers again, once per invoked tool.
		for _, id := range sdkIDs {
			batch.ensureCall(id, "lookup", ClientTool{}, args, "", "")
			broker.Register(batch)
			batch.startTimer()
		}
		batch.Cancel(context.Canceled)
	}
}

// BenchmarkToolTurnRegisterAndClose scales the tool count so the cost of
// closing a batch can be read as linear or quadratic in the number of tool
// calls in the turn.
func BenchmarkToolTurnRegisterAndClose(b *testing.B) {
	for _, tools := range []int{1, 8, 32, 128} {
		b.Run(fmt.Sprintf("tools=%d", tools), func(b *testing.B) {
			benchmarkTurn(b, NewBroker(time.Minute), tools)
		})
	}
}

// BenchmarkBrokerRegisterRepeat isolates Register itself on an already
// registered batch, which is what every handleInvocation after the first does.
func BenchmarkBrokerRegisterRepeat(b *testing.B) {
	broker := NewBroker(time.Minute)
	batch := newBatch(time.Minute, "resp_bench", "responses", "gpt-5", nil, nil, context.Background())
	for i := range 32 {
		batch.ensureCall(fmt.Sprintf("call_%d", i), "lookup", ClientTool{}, json.RawMessage(`{}`), "", "")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		broker.Register(batch)
	}
}

package toolproxy

import (
	"testing"
	"time"
)

// waitForSDKCall polls the broker for the live batch that owns the upstream
// SDK tool-call id sdkID, returning the batch and the proxy-minted tool-call id
// that would have been published to the client.
//
// Client-visible tool-call ids are proxy-owned uuids, so tests that drive SDK
// tool handlers directly cannot predict them and must resolve them through the
// batch's reverse index instead.
func waitForSDKCall(t *testing.T, broker *Broker, sdkID string) (*Batch, string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		broker.mu.Lock()
		batches := make([]*Batch, 0, len(broker.batches))
		for _, batch := range broker.batches {
			batches = append(batches, batch)
		}
		broker.mu.Unlock()
		for _, batch := range batches {
			if call, ok := batch.callBySDKID(sdkID); ok && batch.isOpen() {
				return batch, call.OpenAIID
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no live batch registered for SDK tool-call id %q", sdkID)
	return nil, ""
}

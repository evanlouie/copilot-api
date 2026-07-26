package copilotgw

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
)

// promptLog returns every prompt the runtime received, tagged with the SDK
// session it landed in, in the order the sessions were opened. It is the only
// thing that can answer the question these tests exist for: what did the model
// actually see?
func (f *fakeSDKRuntime) promptLog() []string {
	f.mu.Lock()
	sessions := append([]*fakeSDKSession(nil), f.sessions...)
	f.mu.Unlock()
	var lines []string
	for _, session := range sessions {
		for _, prompt := range session.prompts() {
			lines = append(lines, fmt.Sprintf("%s: %s", session.id, prompt.Prompt))
		}
	}
	return lines
}

func promptLogContains(log []string, want string) bool {
	for _, line := range log {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// A warm response is allowed to continue another warm response: the client gets
// two completed responses and has been told both inputs were accepted. The
// second warm response resumes the first one's SDK session, so neither input has
// been sent yet and both must ride along on the turn that finally generates.
func TestChainedWarmResponsesDeliverEveryHeldInput(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("done")}
	gw := newSDKTestGateway(t, runtime)

	first, err := gw.WarmResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "ALPHA"},
		Store: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The WebSocket transport drops the live warm session before warming again,
	// so the second warm request reaches the gateway with nothing but the first
	// response's id. Model that here.
	first.WarmSession.Disconnect()

	second, err := gw.WarmResponse(context.Background(), ResponseRequest{
		Model:              "gpt-test",
		Input:              openai.PromptContent{Text: "BRAVO"},
		PreviousResponseID: first.Response.ID,
		Store:              true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.WarmSession.Disconnect()

	firstRecord, err := gw.store.LoadResponse(first.Response.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondRecord, err := gw.store.LoadResponse(second.Response.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("W1 record: input=%q pending=%v session=%s", firstRecord.InputText, firstRecord.InputPending, firstRecord.SDKSessionID)
	t.Logf("W2 record: input=%q pending=%v session=%s prev=%s", secondRecord.InputText, secondRecord.InputPending, secondRecord.SDKSessionID, secondRecord.PreviousResponseID)

	stream, err := gw.StreamResponse(context.Background(), ResponseRequest{
		Model:              "gpt-test",
		Input:              openai.PromptContent{Text: "CHARLIE"},
		PreviousResponseID: second.Response.ID,
		WarmSession:        second.WarmSession,
		Store:              true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for event := range stream {
		if event.Kind == "error" {
			t.Fatalf("stream failed: %v", event.Error)
		}
	}

	log := runtime.promptLog()
	t.Logf("prompts actually sent to the runtime: %q", log)
	for _, want := range []string{"ALPHA", "BRAVO", "CHARLIE"} {
		if !promptLogContains(log, want) {
			t.Fatalf("DATA LOSS: %q never reached the model; prompts were %q", want, log)
		}
	}
}

// The chain has to hold up on the durable path too, which is the one a client
// whose WebSocket dropped between warming and generating actually takes: no live
// warm session, nothing but previous_response_id and the records on disk.
func TestChainedWarmInputSurvivesADroppedSocket(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("done")}
	gw := newSDKTestGateway(t, runtime)

	first, err := gw.WarmResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "ALPHA"},
		Store: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	first.WarmSession.Disconnect()
	second, err := gw.WarmResponse(context.Background(), ResponseRequest{
		Model:              "gpt-test",
		Input:              openai.PromptContent{Text: "BRAVO"},
		PreviousResponseID: first.Response.ID,
		Store:              true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The socket is gone, so the live warm session goes with it.
	second.WarmSession.Disconnect()

	// CreateResponse rather than StreamResponse: it sends on the caller's
	// goroutine, so the claims are settled by the time it returns and the
	// assertion below needs no polling. Both paths share prepareResponseTurn and
	// markPendingInputDelivered.
	if _, err := gw.CreateResponse(context.Background(), ResponseRequest{
		Model:              "gpt-test",
		Input:              openai.PromptContent{Text: "CHARLIE"},
		PreviousResponseID: second.Response.ID,
		Store:              true,
	}); err != nil {
		t.Fatal(err)
	}

	log := runtime.promptLog()
	if len(log) != 1 || !strings.HasSuffix(log[0], "ALPHA\n\nBRAVO\n\nCHARLIE") {
		t.Fatalf("prompts sent = %q, want the whole warmed chain ahead of the new turn", log)
	}
	for _, id := range []string{first.Response.ID, second.Response.ID} {
		record, err := gw.store.LoadResponse(id)
		if err != nil {
			t.Fatal(err)
		}
		if record.InputPending {
			t.Fatalf("record %s is still pending after its input was delivered; the next resume would repeat it", id)
		}
	}
}

// The other half of exactly-once: a send that never happened has delivered
// nothing, so the claim must stand and the retry must still carry the warmed
// input.
func TestWarmInputStaysPendingWhenTheSendFails(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("done"), sendErr: errors.New("runtime refused the prompt")}
	gw := newSDKTestGateway(t, runtime)

	warm, err := gw.WarmResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "ALPHA"},
		Store: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	warm.WarmSession.Disconnect()

	if _, err := gw.CreateResponse(context.Background(), ResponseRequest{
		Model:              "gpt-test",
		Input:              openai.PromptContent{Text: "TURN1"},
		PreviousResponseID: warm.Response.ID,
		Store:              true,
	}); err == nil {
		t.Fatal("the turn reported success even though the send failed")
	}
	record, err := gw.store.LoadResponse(warm.Response.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.InputPending {
		t.Fatal("a failed send retired the warm record's claim; the input the client was told had been accepted is now unreachable")
	}

	runtime.mu.Lock()
	runtime.sendErr = nil
	runtime.mu.Unlock()
	if _, err := gw.CreateResponse(context.Background(), ResponseRequest{
		Model:              "gpt-test",
		Input:              openai.PromptContent{Text: "TURN2"},
		PreviousResponseID: warm.Response.ID,
		Store:              true,
	}); err != nil {
		t.Fatal(err)
	}
	log := runtime.promptLog()
	if len(log) != 2 || !strings.HasSuffix(log[1], "ALPHA\n\nTURN2") {
		t.Fatalf("prompts sent = %q, want the retry to carry the warmed input", log)
	}
}

// The durable half of warming exists so a client whose WebSocket dropped can
// carry on from the warm response id. Doing that twice - a retry, or simply
// using the warm id as the conversation anchor - must not push the warmed input
// into the shared SDK session again.
func TestWarmInputIsDeliveredOnceAcrossRepeatedResumes(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("done")}
	gw := newSDKTestGateway(t, runtime)

	warm, err := gw.WarmResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "ALPHA"},
		Store: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The socket dropped, so the live warm session is gone and both generating
	// turns arrive with nothing but previous_response_id.
	warm.WarmSession.Disconnect()

	for _, turn := range []string{"TURN1", "TURN2"} {
		// CreateResponse settles the claims before it returns, so the second turn
		// genuinely observes the first one's outcome rather than racing it.
		if _, err := gw.CreateResponse(context.Background(), ResponseRequest{
			Model:              "gpt-test",
			Input:              openai.PromptContent{Text: turn},
			PreviousResponseID: warm.Response.ID,
			Store:              true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	log := runtime.promptLog()
	t.Logf("prompts sent: %q", log)
	delivered := 0
	for _, line := range log {
		delivered += strings.Count(line, "ALPHA")
	}
	t.Logf("ALPHA delivered %d times", delivered)
	if delivered != 1 {
		t.Fatalf("warmed input reached the session %d times, want exactly 1", delivered)
	}
}

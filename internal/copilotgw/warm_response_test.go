package copilotgw

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	copilot "github.com/github/copilot-sdk/go"
)

// TestStopDisconnectsWarmSessions is the regression for a warm session escaping
// gateway shutdown accounting. A WarmResponseSession owns a live SDK session and
// retention pins but has no turnRunner, so before the gateway tracked it Stop
// walked only the runner registries and left the SDK session connected with its
// pins still held.
func TestStopDisconnectsWarmSessions(t *testing.T) {
	t.Parallel()
	store := sessionstore.New(t.TempDir(), t.TempDir())
	gw := NewReal(config.Config{ToolCallTTL: time.Minute}, store, nil)
	var releases atomic.Int32
	warm := &WarmResponseSession{
		responseID:  "resp_warm",
		sessionID:   "resp_sdk_warm",
		model:       "gpt-5",
		pinReleases: []func(){func() { releases.Add(1) }},
	}
	if !gw.trackWarmSession(warm) {
		t.Fatal("gateway refused to track a warm session before Stop")
	}

	if err := gw.Stop(); err != nil {
		t.Fatal(err)
	}

	if !warm.Disconnected() {
		t.Fatal("Stop left the warm SDK session connected")
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("warm retention pin releases = %d, want exactly 1 by the end of Stop", got)
	}
}

// TestTrackWarmSessionAfterStopIsRejected covers the register-after-close race:
// once Stop has taken its snapshot nothing will ever drain the registry again,
// so registration must fail loudly rather than accept a session that would never
// be cleaned up.
func TestTrackWarmSessionAfterStopIsRejected(t *testing.T) {
	t.Parallel()
	store := sessionstore.New(t.TempDir(), t.TempDir())
	gw := NewReal(config.Config{ToolCallTTL: time.Minute}, store, nil)
	if err := gw.Stop(); err != nil {
		t.Fatal(err)
	}
	var releases atomic.Int32
	warm := &WarmResponseSession{responseID: "resp_late", pinReleases: []func(){func() { releases.Add(1) }}}
	if gw.trackWarmSession(warm) {
		t.Fatal("a warm session registered after Stop; nothing would ever tear it down")
	}
	// Registration was rejected, so the caller still owns the teardown.
	warm.Disconnect()
	if got := releases.Load(); got != 1 {
		t.Fatalf("caller teardown released %d pins, want 1", got)
	}
}

// TestWarmSessionUseDeregistersFromGatewayShutdown pins the ownership handoff:
// use transfers the SDK session to a turnRunner, which the active registry
// already accounts for, so Stop must not disconnect it a second time.
func TestWarmSessionUseDeregistersFromGatewayShutdown(t *testing.T) {
	t.Parallel()
	store := sessionstore.New(t.TempDir(), t.TempDir())
	gw := NewReal(config.Config{ToolCallTTL: time.Minute}, store, nil)
	var releases atomic.Int32
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", pinReleases: []func(){func() { releases.Add(1) }}}
	if !gw.trackWarmSession(warm) {
		t.Fatal("gateway refused to track a warm session")
	}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm"}
	used, ok := warm.use(&req)
	if !ok {
		t.Fatal("warm session was not used")
	}
	if len(used.pinReleases) != 1 {
		t.Fatalf("use transferred %d pin releases, want 1", len(used.pinReleases))
	}
	if gw.warm.tracked(warm) {
		t.Fatal("use left the warm session registered for gateway shutdown")
	}
	if err := gw.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := releases.Load(); got != 0 {
		t.Fatalf("Stop released %d pins the turn now owns, want 0", got)
	}
}

// TestWarmSessionDisconnectIsIdempotentUnderConcurrentStop interleaves a client
// disconnect with gateway shutdown, which is the exact race the disconnected
// flag exists to make safe: pins must be released exactly once.
func TestWarmSessionDisconnectIsIdempotentUnderConcurrentStop(t *testing.T) {
	t.Parallel()
	store := sessionstore.New(t.TempDir(), t.TempDir())
	gw := NewReal(config.Config{ToolCallTTL: time.Minute}, store, nil)
	var releases atomic.Int32
	warm := &WarmResponseSession{responseID: "resp_warm", pinReleases: []func(){func() { releases.Add(1) }}}
	if !gw.trackWarmSession(warm) {
		t.Fatal("gateway refused to track a warm session")
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		warm.Disconnect()
	}()
	var stopErr error
	go func() {
		defer wg.Done()
		<-start
		stopErr = gw.Stop()
	}()
	close(start)
	wg.Wait()

	if stopErr != nil {
		t.Fatal(stopErr)
	}
	if !warm.Disconnected() {
		t.Fatal("warm session survived a concurrent client disconnect and gateway stop")
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("warm retention pin releases = %d, want exactly 1", got)
	}
}

func TestWarmResponseSessionUseInheritsWarmRequestState(t *testing.T) {
	t.Parallel()
	warm := &WarmResponseSession{
		responseID:      "resp_warm",
		model:           "gpt-5",
		instructions:    "Be concise.",
		reasoningEffort: "low",
		tools:           []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "lookup"}},
		input:           resolvedPrompt{Text: "Warm context"},
	}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Input: openai.PromptContent{Text: "Current turn"}}

	used, ok := warm.use(&req)
	if !ok {
		t.Fatal("warm session was not used")
	}
	if used.previous == nil || *used.previous != "resp_warm" {
		t.Fatalf("previous = %#v, want resp_warm", used.previous)
	}
	if req.Instructions != "Be concise." || req.ReasoningEffort != "low" {
		t.Fatalf("request did not inherit instructions/reasoning: %#v", req)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Fatalf("request tools = %#v, want warm lookup tool", req.Tools)
	}
	combined := combineResolvedPrompts(used.prompt, resolvedPrompt{Text: req.Input.Text})
	if combined.Text != "Warm context\n\nCurrent turn" {
		t.Fatalf("combined input = %q", combined.Text)
	}
}

func TestWarmResponseSessionTransfersResolvedImagesAndModelCount(t *testing.T) {
	t.Parallel()
	data := "aW1hZ2U="
	attachment := copilot.AttachmentBlob{Data: &data, MIMEType: "image/png"}
	budget := &imageRequestBudget{configured: true, maxImages: 2, remainingImages: 1}
	pinReleased := false
	warm := &WarmResponseSession{
		responseID: "resp_warm", model: "gpt-5",
		input:       resolvedPrompt{Text: "warm", Attachments: []copilot.Attachment{attachment}},
		imageBudget: budget, pinReleases: []func(){func() { pinReleased = true }},
	}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm"}
	used, ok := warm.use(&req)
	if !ok || len(used.prompt.Attachments) != 1 || used.imageBudget != budget || len(used.pinReleases) != 1 {
		t.Fatalf("warm transfer = %#v, ok=%v", used, ok)
	}
	combined := combineResolvedPrompts(used.prompt, resolvedPrompt{Attachments: []copilot.Attachment{attachment}})
	if len(combined.Attachments) != 2 {
		t.Fatalf("combined attachments = %d", len(combined.Attachments))
	}
	releaseAll(used.pinReleases)
	if !pinReleased {
		t.Fatal("pin ownership was not released")
	}
}

func TestRejectedWarmResponseDisconnectReleasesPins(t *testing.T) {
	t.Parallel()
	pinReleased := false
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", pinReleases: []func(){func() { pinReleased = true }}}
	req := ResponseRequest{Model: "other", PreviousResponseID: "resp_warm"}
	if _, ok := warm.use(&req); ok {
		t.Fatal("mismatched warm response was used")
	}
	warm.Disconnect()
	if !pinReleased {
		t.Fatal("warm response pin was not released")
	}
}

func TestWarmResponseSessionUseInheritsResolvedDynamicCatalog(t *testing.T) {
	t.Parallel()
	warm := &WarmResponseSession{
		responseID: "resp_warm",
		model:      "gpt-5",
		tools: []toolcatalog.NormalizedTool{
			{Kind: toolcatalog.ToolKindToolSearch, Name: "tool_search", Execution: "client"},
			{Kind: toolcatalog.ToolKindNamespace, Name: "multi_agent_v1", Children: []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "spawn_agent"}}},
		},
	}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm"}
	_, ok := warm.use(&req)
	if !ok {
		t.Fatal("warm session was not used")
	}
	if len(req.Tools) != 2 || req.Tools[1].Kind != toolcatalog.ToolKindNamespace || req.Tools[1].Children[0].Name != "spawn_agent" {
		t.Fatalf("request tools = %#v, want resolved dynamic catalog", req.Tools)
	}
}

func TestWarmResponseSessionUseAcceptsSemanticEquivalentToolCatalog(t *testing.T) {
	t.Parallel()
	warm := &WarmResponseSession{
		responseID: "resp_warm",
		model:      "gpt-5",
		tools: []toolcatalog.NormalizedTool{
			{Kind: toolcatalog.ToolKindFunction, Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`), Raw: json.RawMessage(`{"type":"function","name":"lookup","parameters":{"properties":{"q":{"type":"string"}},"type":"object"}}`)},
			{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"syntax":"lark","type":"grammar"}`), Raw: json.RawMessage(`{"name":"apply_patch","type":"custom"}`)},
		},
	}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Tools: []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"type":"grammar","syntax":"lark"}`), Raw: json.RawMessage(`{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark"}}`)},
		{Kind: toolcatalog.ToolKindFunction, Name: "lookup", Parameters: json.RawMessage(` { "properties" : { "q" : { "type" : "string" } }, "type" : "object" } `), Raw: json.RawMessage(`{"different":"raw should not affect reuse"}`)},
	}}
	if _, ok := warm.use(&req); !ok {
		t.Fatal("warm session was not used for semantically equivalent tool catalog")
	}
}

func TestWarmResponseSessionUseRejectsSemanticToolCatalogMismatch(t *testing.T) {
	t.Parallel()
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", tools: []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"type":"grammar","syntax":"lark"}`)}}}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Tools: []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"type":"grammar","syntax":"regex"}`)}}}
	if _, ok := warm.use(&req); ok {
		t.Fatal("warm session used despite semantic tool catalog mismatch")
	}
}

func TestWarmResponseSessionUseAcceptsEquivalentExplicitReasoningEffort(t *testing.T) {
	t.Parallel()
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", reasoningEffort: "low"}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", ReasoningEffort: " LOW "}
	if _, ok := warm.use(&req); !ok {
		t.Fatal("warm session was not used for equivalent explicit reasoning effort")
	}
}

func TestWarmResponseSessionUseRejectsMismatchedReasoningEffort(t *testing.T) {
	t.Parallel()
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", reasoningEffort: "low"}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", ReasoningEffort: "high"}
	if _, ok := warm.use(&req); ok {
		t.Fatal("warm session used despite mismatched reasoning effort")
	}
}

func TestWarmResponseSessionUseRejectsMismatchedInstructions(t *testing.T) {
	t.Parallel()
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", instructions: "original"}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Instructions: "changed"}
	if _, ok := warm.use(&req); ok {
		t.Fatal("warm session used despite mismatched instructions")
	}
}

// A warm session's SDK session was configured with the catalog its own
// tool_choice implied and cannot be reconfigured, so serving a request that
// asks for a different catalog would silently give the client a tool scope it
// did not ask for. Refusing sends the caller down the resume path, which
// rebuilds the catalog this request actually named.
func TestWarmResponseSessionUseRejectsMismatchedToolChoiceScope(t *testing.T) {
	t.Parallel()
	tools := []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindFunction, Name: "lookup"},
		{Kind: toolcatalog.ToolKindFunction, Name: "get_weather"},
	}
	tests := map[string]struct {
		warm    openai.ToolChoice
		req     openai.ToolChoice
		reusing bool
	}{
		"narrowed then widened":  {warm: openai.ToolChoice{Kind: "function", Name: "lookup"}, req: openai.ToolChoice{Kind: "auto"}},
		"widened then narrowed":  {warm: openai.ToolChoice{Kind: "auto"}, req: openai.ToolChoice{Kind: "function", Name: "lookup"}},
		"different forced tools": {warm: openai.ToolChoice{Kind: "function", Name: "lookup"}, req: openai.ToolChoice{Kind: "function", Name: "get_weather"}},
		"none then auto":         {warm: openai.ToolChoice{Kind: "none"}, req: openai.ToolChoice{Kind: "auto"}},
		"same forced tool":       {warm: openai.ToolChoice{Kind: "function", Name: "lookup"}, req: openai.ToolChoice{Kind: "function", Name: "lookup"}, reusing: true},
		// A kind-specific forced scope and a name-only allow-list are not generally
		// interchangeable: a valid mixed-kind catalog can narrow them differently.
		"kind-specific versus name-only": {warm: openai.ToolChoice{Kind: "function", Name: "lookup"}, req: openai.ToolChoice{Kind: "allowed_tools", AllowedMode: "auto", Allowed: []string{"lookup"}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", tools: tools, toolChoice: tt.warm}
			req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", ToolChoice: tt.req}
			if _, ok := warm.use(&req); ok != tt.reusing {
				t.Fatalf("warm reuse = %t, want %t", ok, tt.reusing)
			}
		})
	}
}

// An unset tool_choice inherits the warm session's, exactly as instructions and
// reasoning effort do: the session's catalog is the one the turn will run with,
// so the request must be told what it is.
func TestWarmResponseSessionUseInheritsToolChoice(t *testing.T) {
	t.Parallel()
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", toolChoice: openai.ToolChoice{Kind: "none"}}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm"}
	if _, ok := warm.use(&req); !ok {
		t.Fatal("warm session was not used for an unset tool_choice")
	}
	if req.ToolChoice.Kind != "none" {
		t.Fatalf("inherited tool_choice = %#v, want the warm session's none", req.ToolChoice)
	}
}

// TestPendingWarmInputSurvivesResume covers the durable half of warming: a warm
// response buffers its input in memory only, so if the client's WebSocket drops
// before it generates, the resumed SDK session has never seen that input. The
// record marks it undelivered so the resume replays it ahead of the new turn.
func TestPendingWarmInputSurvivesResume(t *testing.T) {
	t.Parallel()
	gw := newSDKTestGateway(t, &fakeSDKRuntime{})
	record := sessionstore.ResponseRecord{ID: "resp_warm", SDKSessionID: "sess_warm", InputText: "Warm context", InputPending: true}
	pending := gw.pendingInputForSession(record)
	combined := combineResolvedPrompts(pending.prompt, resolvedPrompt{Text: "Current turn"})
	if combined.Text != "Warm context\n\nCurrent turn" {
		t.Fatalf("resumed prompt = %q, want the warmed input ahead of the current turn", combined.Text)
	}
	if len(pending.responseIDs) != 1 || pending.responseIDs[0] != "resp_warm" {
		t.Fatalf("pending records = %q, want the warm record the prompt replays", pending.responseIDs)
	}
}

// TestDeliveredInputIsNotReplayedOnResume is the other half: every non-warm
// record's input already reached its SDK session, so resuming must not send it
// again. Records written before input_pending existed decode as false and so
// take this path.
func TestDeliveredInputIsNotReplayedOnResume(t *testing.T) {
	t.Parallel()
	gw := newSDKTestGateway(t, &fakeSDKRuntime{})
	for name, record := range map[string]sessionstore.ResponseRecord{
		"delivered":     {ID: "resp_prev", SDKSessionID: "sess_prev", InputText: "Already sent"},
		"pending blank": {ID: "resp_warm", SDKSessionID: "sess_warm", InputText: "   ", InputPending: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			combined := combineResolvedPrompts(gw.pendingInputForSession(record).prompt, resolvedPrompt{Text: "Current turn"})
			if combined.Text != "Current turn" {
				t.Fatalf("resumed prompt = %q, want only the current turn", combined.Text)
			}
		})
	}
}

package toolproxy

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	copilot "github.com/github/copilot-sdk/go"
)

func TestResponseRequestToolsFlattenExtendedResponsesTools(t *testing.T) {
	t.Parallel()
	tools := []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindFunction, Name: "multi_tool_use.parallel", Description: "parallel", Parameters: []byte(`{"type":"object","properties":{}}`)},
		{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch", Description: "patch"},
		{Kind: toolcatalog.ToolKindNamespace, Name: "mcp__grep_app", Children: []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "searchGitHub", Description: "search", Parameters: []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`)}}},
		{Kind: toolcatalog.ToolKindToolSearch, Name: "tool_search", Execution: "client", Parameters: []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`)},
	}
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), tools, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Tools()) != 4 {
		t.Fatalf("SDK tools = %#v, want four", rt.Tools())
	}
	gotNames := make([]string, 0, len(rt.Tools()))
	for _, tool := range rt.Tools() {
		gotNames = append(gotNames, tool.Name)
		if strings.Contains(tool.Name, ".") {
			t.Fatalf("SDK tool name %q contains a dot", tool.Name)
		}
	}
	for _, want := range []string{"apply_patch", "mcp__grep_app__searchGitHub", "tool_search"} {
		if !contains(gotNames, want) {
			t.Fatalf("SDK tools = %#v, missing %q", gotNames, want)
		}
	}
	if contains(gotNames, "multi_tool_use.parallel") {
		t.Fatalf("unsafe function name was not aliased: %#v", gotNames)
	}
	if got := rt.AvailableTools(); !contains(got, "custom:apply_patch") || !contains(got, "custom:mcp__grep_app__searchGitHub") || !contains(got, "custom:tool_search") {
		t.Fatalf("available tools = %#v, missing expected custom filters", got)
	}
}

func TestCaptureRequestsRehydratesExtendedResponseToolMetadata(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewResponseRequestTools(broker, []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"},
		{Kind: toolcatalog.ToolKindNamespace, Name: "mcp__grep_app", Children: []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "searchGitHub"}}},
		{Kind: toolcatalog.ToolKindToolSearch, Name: "tool_search", Execution: "client"},
	}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	batch, calls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{
		{ToolCallID: "call_patch", Name: "apply_patch", Arguments: map[string]any{"input": "*** Begin Patch\n*** End Patch"}},
		{ToolCallID: "call_mcp", Name: "mcp__grep_app__searchGitHub", Arguments: map[string]any{"query": "repo:test"}},
		{ToolCallID: "call_search", Name: "tool_search", Arguments: map[string]any{"query": "grep"}},
	}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if batch == nil || len(calls) != 3 {
		t.Fatalf("batch/calls = %#v %#v, want three calls", batch, calls)
	}
	byID := map[string]CapturedCall{}
	for _, call := range calls {
		byID[call.SDKName] = call
	}
	if got := byID["apply_patch"]; got.Kind != toolcatalog.ToolKindCustom || got.ResponseName != "apply_patch" || got.Input != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom captured call = %#v", got)
	}
	if got := byID["mcp__grep_app__searchGitHub"]; got.Kind != toolcatalog.ToolKindFunction || got.Namespace != "mcp__grep_app" || got.ResponseName != "searchGitHub" || string(got.ArgumentsJSON) != `{"query":"repo:test"}` {
		t.Fatalf("namespace captured call = %#v", got)
	}
	if got := byID["tool_search"]; got.Kind != toolcatalog.ToolKindToolSearch || got.Execution != "client" || string(got.ArgumentsJSON) != `{"query":"grep"}` {
		t.Fatalf("tool_search captured call = %#v", got)
	}
}

func TestResponseRequestToolsRejectSDKAliasCollisions(t *testing.T) {
	t.Parallel()
	_, err := FlattenResponsesTools([]toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindFunction, Name: "lookup"},
		{Kind: toolcatalog.ToolKindCustom, Name: "lookup"},
	})
	if err == nil || !strings.Contains(err.Error(), "SDK tool name collision") {
		t.Fatalf("error = %v, want SDK collision", err)
	}
}

func TestToolChoiceNoneWithExtendedResponsesToolsUsesSentinel(t *testing.T) {
	t.Parallel()
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"}}, openai.ToolScope{None: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Tools()) != 0 {
		t.Fatalf("SDK tools = %#v, want none", rt.Tools())
	}
	if got, want := rt.AvailableTools(), []string{NoToolsSentinel}; !reflect.DeepEqual(got, want) {
		t.Fatalf("available tools = %#v, want %#v", got, want)
	}
	if _, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_bad", Name: NoToolsSentinel, Arguments: map[string]any{}}}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil); err == nil {
		t.Fatal("expected sentinel tool request to be rejected")
	}
}

func TestRequestToolsRejectUnconfiguredSDKToolRequestsAndInvocations(t *testing.T) {
	t.Parallel()
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "lookup"}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_bad", Name: "read_file", Arguments: map[string]any{}}}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil); err == nil || !strings.Contains(err.Error(), "unconfigured SDK tool request") {
		t.Fatalf("error = %v, want unconfigured request rejection", err)
	}
	if _, err := rt.handleInvocation(copilot.ToolInvocation{ToolCallID: "call_bad", ToolName: "read_file", Arguments: map[string]any{}}); err == nil || !strings.Contains(err.Error(), "unconfigured SDK tool invocation") {
		t.Fatalf("error = %v, want unconfigured invocation rejection", err)
	}
}

func TestExtendedToolOutputKindMustMatchPendingCall(t *testing.T) {
	t.Parallel()
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	batch, calls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_patch", Name: "apply_patch", Arguments: map[string]any{"input": "patch"}}}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	callID := calls[0].CallID
	wrongKind := map[string]toolcatalog.ResponseToolOutput{callID: {Kind: toolcatalog.ToolKindFunction, CallID: callID, Output: "ok"}}
	if err := batch.CompleteToolOutputsWithSetup(wrongKind, nil); err == nil || !strings.Contains(err.Error(), "output does not match pending") {
		t.Fatalf("error = %v, want kind mismatch", err)
	}
}

func TestCustomToolOutputNameMustMatchPendingCallWhenPresent(t *testing.T) {
	t.Parallel()
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	batch, calls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_patch", Name: "apply_patch", Arguments: map[string]any{"input": "patch"}}}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	callID := calls[0].CallID
	bad := map[string]toolcatalog.ResponseToolOutput{callID: {Kind: toolcatalog.ToolKindCustom, CallID: callID, Name: "wrong_tool", Output: "ok"}}
	if err := batch.CompleteToolOutputsWithSetup(bad, nil); err == nil || !strings.Contains(err.Error(), "does not match pending custom tool") {
		t.Fatalf("error = %v, want custom name mismatch", err)
	}
	good := map[string]toolcatalog.ResponseToolOutput{callID: {Kind: toolcatalog.ToolKindCustom, CallID: callID, Name: "apply_patch", Output: "ok"}}
	if err := batch.CompleteToolOutputsWithSetup(good, nil); err != nil {
		t.Fatalf("matching custom output name should complete: %v", err)
	}
}

func TestToolSearchOutputToolsDoNotMutateLiveAvailableTools(t *testing.T) {
	t.Parallel()
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindToolSearch, Name: "tool_search", Execution: "client"}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	batch, calls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_search", Name: "tool_search", Arguments: map[string]any{"query": "load"}}}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	callID := calls[0].CallID
	before := append([]string{}, rt.AvailableTools()...)
	outputs := map[string]toolcatalog.ResponseToolOutput{callID: {Kind: toolcatalog.ToolKindToolSearch, CallID: callID, Execution: "client", Output: `[{"type":"function","name":"loaded_tool"}]`, Tools: []byte(`[{"type":"function","name":"loaded_tool"}]`)}}
	if err := batch.CompleteToolOutputsWithSetup(outputs, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rt.AvailableTools(), before) {
		t.Fatalf("AvailableTools changed after tool_search_output: before=%#v after=%#v", before, rt.AvailableTools())
	}
	if contains(rt.AvailableTools(), "custom:loaded_tool") {
		t.Fatalf("returned tool was exposed in live AvailableTools: %#v", rt.AvailableTools())
	}
}

// allowed_tools is exactly a catalog restriction, so filtering satisfies it
// rather than approximating it: the permitted tool stays configured and
// everything else stops being visible to the model at all.
func TestAllowedToolsNarrowsTheResponsesCatalog(t *testing.T) {
	t.Parallel()
	tools := []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindFunction, Name: "lookup"},
		{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"},
		{Kind: toolcatalog.ToolKindNamespace, Name: "mcp__grep_app", Children: []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "searchGitHub"}}},
	}
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), tools, openai.ToolScope{Only: []string{"apply_patch", "mcp__grep_app.searchGitHub"}})
	if err != nil {
		t.Fatal(err)
	}
	got := rt.AvailableTools()
	if !contains(got, "custom:apply_patch") || !contains(got, "custom:mcp__grep_app__searchGitHub") {
		t.Fatalf("available tools = %#v, want the two allowed tools", got)
	}
	if contains(got, "custom:lookup") {
		t.Fatalf("available tools = %#v, want lookup withheld", got)
	}
	if len(rt.Tools()) != 2 {
		t.Fatalf("SDK tools = %#v, want only the allowed two", rt.Tools())
	}
}

// Narrowing must not rename anything: SDK names and aliases are assigned over
// the catalog the client declared, so a tool keeps the name a wider request
// would have given it and the calls it makes still rehydrate.
func TestForcedToolChoiceExposesOnlyThatToolWithoutRenamingIt(t *testing.T) {
	t.Parallel()
	tools := []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindFunction, Name: "multi_tool_use.parallel"},
		{Kind: toolcatalog.ToolKindFunction, Name: "lookup"},
	}
	wide, err := NewResponseRequestTools(NewBroker(time.Minute), tools, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	forced, err := NewResponseRequestTools(NewBroker(time.Minute), tools, openai.ToolScope{Only: []string{"multi_tool_use.parallel"}, Forced: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(forced.Tools()) != 1 {
		t.Fatalf("SDK tools = %#v, want only the forced tool", forced.Tools())
	}
	var wideName string
	for _, tool := range wide.Tools() {
		if strings.HasPrefix(tool.Name, "multi_tool_use") {
			wideName = tool.Name
		}
	}
	if wideName == "" || forced.Tools()[0].Name != wideName {
		t.Fatalf("forced SDK name = %q, want the unnarrowed alias %q", forced.Tools()[0].Name, wideName)
	}
}

// OpenAI rejects a forced choice for a tool the request never declared, and so
// must this: narrowing to nothing leaves the model an empty catalog and no way
// to do what was asked. The blame has to land on tool_choice, not on tools.
func TestForcedToolChoiceForAnUnknownToolIsRejected(t *testing.T) {
	t.Parallel()
	_, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "lookup"}}, openai.ToolScope{Only: []string{"missing"}, Forced: true})
	var domain *apierr.Error
	if !errors.As(err, &domain) || domain.Kind != apierr.KindInvalidInput || domain.Param != "tool_choice" {
		t.Fatalf("error = %#v, want an invalid_request blaming tool_choice", err)
	}
	if !strings.Contains(domain.Message, "missing") {
		t.Fatalf("message = %q, want the unknown tool named", domain.Message)
	}
}

// An allow-list matching nothing is a filter that permits nothing, not a client
// error: a Responses catalog can still grow through tool_search, so the honest
// outcome is the same withheld catalog tool_choice: "none" produces.
func TestAllowedToolsMatchingNothingWithholdsTheCatalog(t *testing.T) {
	t.Parallel()
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "lookup"}}, openai.ToolScope{Only: []string{"not_installed_yet"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rt.AvailableTools(), []string{NoToolsSentinel}; !reflect.DeepEqual(got, want) {
		t.Fatalf("available tools = %#v, want %#v", got, want)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

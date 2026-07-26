package toolproxy

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	copilot "github.com/github/copilot-sdk/go"
)

func TestResponseRequestToolsFlattenExtendedResponsesTools(t *testing.T) {
	tools := []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindFunction, Name: "multi_tool_use.parallel", Description: "parallel", Parameters: []byte(`{"type":"object","properties":{}}`)},
		{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch", Description: "patch"},
		{Kind: toolcatalog.ToolKindNamespace, Name: "mcp__grep_app", Children: []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "searchGitHub", Description: "search", Parameters: []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`)}}},
		{Kind: toolcatalog.ToolKindToolSearch, Name: "tool_search", Execution: "client", Parameters: []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`)},
	}
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), tools, false)
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
	broker := NewBroker(time.Minute)
	rt, err := NewResponseRequestTools(broker, []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"},
		{Kind: toolcatalog.ToolKindNamespace, Name: "mcp__grep_app", Children: []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "searchGitHub"}}},
		{Kind: toolcatalog.ToolKindToolSearch, Name: "tool_search", Execution: "client"},
	}, false)
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
		byID[call.CallID] = call
	}
	if got := byID["call_patch"]; got.Kind != toolcatalog.ToolKindCustom || got.ResponseName != "apply_patch" || got.Input != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom captured call = %#v", got)
	}
	if got := byID["call_mcp"]; got.Kind != toolcatalog.ToolKindFunction || got.Namespace != "mcp__grep_app" || got.ResponseName != "searchGitHub" || string(got.ArgumentsJSON) != `{"query":"repo:test"}` {
		t.Fatalf("namespace captured call = %#v", got)
	}
	if got := byID["call_search"]; got.Kind != toolcatalog.ToolKindToolSearch || got.Execution != "client" || string(got.ArgumentsJSON) != `{"query":"grep"}` {
		t.Fatalf("tool_search captured call = %#v", got)
	}
}

func TestResponseRequestToolsRejectSDKAliasCollisions(t *testing.T) {
	_, err := FlattenResponsesTools([]toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindFunction, Name: "lookup"},
		{Kind: toolcatalog.ToolKindCustom, Name: "lookup"},
	})
	if err == nil || !strings.Contains(err.Error(), "SDK tool name collision") {
		t.Fatalf("error = %v, want SDK collision", err)
	}
}

func TestToolChoiceNoneWithExtendedResponsesToolsUsesSentinel(t *testing.T) {
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"}}, true)
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
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "lookup"}}, false)
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
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	batch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_patch", Name: "apply_patch", Arguments: map[string]any{"input": "patch"}}}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	wrongKind := map[string]toolcatalog.ResponseToolOutput{"call_patch": {Kind: toolcatalog.ToolKindFunction, CallID: "call_patch", Output: "ok"}}
	if err := batch.CompleteToolOutputsWithSetup(wrongKind, nil); err == nil || !strings.Contains(err.Error(), "output does not match pending") {
		t.Fatalf("error = %v, want kind mismatch", err)
	}
}

func TestCustomToolOutputNameMustMatchPendingCallWhenPresent(t *testing.T) {
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	batch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_patch", Name: "apply_patch", Arguments: map[string]any{"input": "patch"}}}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]toolcatalog.ResponseToolOutput{"call_patch": {Kind: toolcatalog.ToolKindCustom, CallID: "call_patch", Name: "wrong_tool", Output: "ok"}}
	if err := batch.CompleteToolOutputsWithSetup(bad, nil); err == nil || !strings.Contains(err.Error(), "does not match pending custom tool") {
		t.Fatalf("error = %v, want custom name mismatch", err)
	}
	good := map[string]toolcatalog.ResponseToolOutput{"call_patch": {Kind: toolcatalog.ToolKindCustom, CallID: "call_patch", Name: "apply_patch", Output: "ok"}}
	if err := batch.CompleteToolOutputsWithSetup(good, nil); err != nil {
		t.Fatalf("matching custom output name should complete: %v", err)
	}
}

func TestToolSearchOutputToolsDoNotMutateLiveAvailableTools(t *testing.T) {
	rt, err := NewResponseRequestTools(NewBroker(time.Minute), []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindToolSearch, Name: "tool_search", Execution: "client"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	batch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_search", Name: "tool_search", Arguments: map[string]any{"query": "load"}}}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	before := append([]string{}, rt.AvailableTools()...)
	outputs := map[string]toolcatalog.ResponseToolOutput{"call_search": {Kind: toolcatalog.ToolKindToolSearch, CallID: "call_search", Execution: "client", Output: `[{"type":"function","name":"loaded_tool"}]`, Tools: []byte(`[{"type":"function","name":"loaded_tool"}]`)}}
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

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

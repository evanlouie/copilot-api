package copilotgw

import (
	"encoding/json"
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
)

func TestResponseFromTurnRehydratesExtendedToolCallItems(t *testing.T) {
	turn := &TurnResult{FinishReason: "tool_calls", ResponseToolCalls: []toolproxy.CapturedCall{
		{Kind: openai.ToolKindFunction, CallID: "call_fn", ResponseName: "multi_tool_use.parallel", ArgumentsJSON: json.RawMessage(`{"tool_uses":[]}`)},
		{Kind: openai.ToolKindFunction, CallID: "call_mcp", Namespace: "mcp__grep_app", ResponseName: "searchGitHub", ArgumentsJSON: json.RawMessage(`{"query":"x"}`)},
		{Kind: openai.ToolKindCustom, CallID: "call_patch", ResponseName: "apply_patch", Input: "*** Begin Patch\n*** End Patch"},
		{Kind: openai.ToolKindToolSearch, CallID: "call_search", Execution: "client", ArgumentsJSON: json.RawMessage(`{"query":"grep"}`)},
	}}
	resp := responseFromTurn("resp_1", "gpt-test", "", nil, true, turn, false)
	if len(resp.Output) != 4 {
		t.Fatalf("output = %#v, want four tool items", resp.Output)
	}
	if item := resp.Output[0]; item.Type != "function_call" || item.Name != "multi_tool_use.parallel" || item.Arguments != `{"tool_uses":[]}` {
		t.Fatalf("function item = %#v", item)
	}
	if item := resp.Output[1]; item.Type != "function_call" || item.Namespace != "mcp__grep_app" || item.Name != "searchGitHub" {
		t.Fatalf("namespace item = %#v", item)
	}
	if item := resp.Output[2]; item.Type != "custom_tool_call" || item.Name != "apply_patch" || item.Input == "" {
		t.Fatalf("custom item = %#v", item)
	}
	if item := resp.Output[3]; item.Type != "tool_search_call" || item.Execution != "client" || string(item.ArgumentsJSON) != `{"query":"grep"}` {
		t.Fatalf("tool_search item = %#v", item)
	}
	b, err := json.Marshal(resp.Output[3])
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"id":"tsc_call_search","type":"tool_search_call","status":"completed","call_id":"call_search","execution":"client","arguments":{"query":"grep"}}` {
		t.Fatalf("tool_search JSON = %s", got)
	}
}

package copilotgw

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/evanlouie/copilot-api/internal/hydration"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionfs"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
)

// responseContinuationChainLimit bounds how far back a cold continuation
// replays a previous_response_id chain.
const responseContinuationChainLimit = 20

// errNoResponseHistory reports that a record chain carried nothing worth
// replaying, so there is no point writing a synthetic session for it.
var errNoResponseHistory = errors.New("response continuation has no replayable history")

// responseContinuationChain walks previous_response_id back from `previous` and
// returns the chain oldest-first.
func (g *RealGateway) responseContinuationChain(previous sessionstore.ResponseRecord) []sessionstore.ResponseRecord {
	records := []sessionstore.ResponseRecord{previous}
	seen := map[string]struct{}{previous.ID: {}}
	for id := previous.PreviousResponseID; id != "" && len(records) < responseContinuationChainLimit; {
		if _, ok := seen[id]; ok {
			break
		}
		seen[id] = struct{}{}
		record, err := g.store.LoadResponseForContinuation(id)
		if err != nil || record.Deleted {
			break
		}
		records = append(records, record)
		id = record.PreviousResponseID
	}
	slices.Reverse(records)
	return records
}

// hydrateResponseContinuation replays a Responses record chain into a cold SDK
// session as real Copilot session events, exactly the way Chat Completions has
// always rebuilt a conversation. The SDK then resumes onto genuine prior turns
// instead of reading a prose transcript pasted into a single user prompt.
func (g *RealGateway) hydrateResponseContinuation(sessionID, model string, previous sessionstore.ResponseRecord) error {
	messages := responseContinuationHistory(g.responseContinuationChain(previous))
	if len(messages) == 0 {
		return errNoResponseHistory
	}
	h, err := hydration.BuildChatHistoryJSONL(messages, hydration.Options{SessionID: sessionID, Model: model})
	if err != nil {
		return err
	}
	if _, err := sessionfs.WriteEvents(g.cfg.DataDir, sessionID, h.JSONL); err != nil {
		return err
	}
	return nil
}

// responseContinuationHistory maps a Responses record chain onto the
// hydration.Message vocabulary shared with Chat Completions.
//
// The Responses surface is the wider one, so a few of its items have no session
// event that expresses them: custom/tool-search calls and their outputs, loaded
// tool-search catalogs, and function calls the client never answered (replaying
// those as SDK tool requests would leave the resumed session waiting on results
// that never arrive). Those degrade to plain text carried on the neighbouring
// message; everything else - user input, assistant text, plaintext reasoning,
// answered function calls and their outputs - replays as a real event.
func responseContinuationHistory(records []sessionstore.ResponseRecord) []hydration.Message {
	answered := map[string]struct{}{}
	for _, record := range records {
		for _, output := range record.ToolOutputs {
			if storedToolOutputIsFunction(output) && output.CallID != "" {
				answered[output.CallID] = struct{}{}
			}
		}
	}
	messages := make([]hydration.Message, 0, len(records)*3)
	declared := map[string]struct{}{}
	for _, record := range records {
		messages = appendResponseRecordHistory(messages, record, answered, declared)
	}
	return messages
}

func appendResponseRecordHistory(messages []hydration.Message, record sessionstore.ResponseRecord, answered, declared map[string]struct{}) []hydration.Message {
	// A record's tool outputs answer the *previous* record's calls, so they come
	// first in wall-clock order.
	var notes []string
	for _, output := range record.ToolOutputs {
		if _, ok := declared[output.CallID]; ok && storedToolOutputIsFunction(output) {
			messages = append(messages, hydration.Message{Role: "tool", ToolCallID: output.CallID, Content: output.Output})
			continue
		}
		notes = append(notes, storedToolOutputPrompt(output))
	}
	for _, event := range record.LoadedToolEvents {
		if len(event.LoadedTools) == 0 {
			continue
		}
		notes = append(notes, "Loaded tools from tool search "+event.SourceCallID+": "+storedToolNames(event.LoadedTools))
	}
	if len(notes) > 0 {
		messages = append(messages, hydration.Message{Role: "user", Content: strings.Join(notes, "\n\n")})
	}
	if strings.TrimSpace(record.InputText) != "" {
		messages = append(messages, hydration.Message{Role: "user", Content: record.InputText})
	}
	assistant := hydration.Message{Role: "assistant", Content: record.OutputText, Reasoning: responseRecordReasoning(record)}
	var summaries []string
	for _, item := range record.Output {
		switch item.Type {
		case "function_call":
			if call, ok := hydratableFunctionCall(item, answered); ok {
				assistant.ToolCalls = append(assistant.ToolCalls, call)
				declared[item.CallID] = struct{}{}
				continue
			}
			summaries = append(summaries, responseCallSummary(item))
		case "custom_tool_call", "tool_search_call":
			summaries = append(summaries, responseCallSummary(item))
		}
	}
	if len(summaries) > 0 {
		joined := strings.Join(summaries, "\n")
		if strings.TrimSpace(assistant.Content) == "" {
			assistant.Content = joined
		} else {
			assistant.Content = assistant.Content + "\n\n" + joined
		}
	}
	if assistant.Content != "" || assistant.Reasoning != "" || len(assistant.ToolCalls) > 0 {
		messages = append(messages, assistant)
	}
	return messages
}

// hydratableFunctionCall reports whether a stored function_call can be replayed
// as a real SDK tool request. It must name a call the client actually answered
// and carry arguments hydration can decode as a single JSON value.
func hydratableFunctionCall(item openai.ResponseOutputItem, answered map[string]struct{}) (openai.ChatToolCall, bool) {
	if item.CallID == "" {
		return openai.ChatToolCall{}, false
	}
	if _, ok := answered[item.CallID]; !ok {
		return openai.ChatToolCall{}, false
	}
	args := item.Arguments
	if args == "" && len(item.ArgumentsJSON) > 0 {
		args = string(item.ArgumentsJSON)
	}
	if args != "" && !json.Valid([]byte(args)) {
		return openai.ChatToolCall{}, false
	}
	name := item.Name
	if item.Namespace != "" && name != "" {
		name = item.Namespace + "." + name
	}
	return openai.ChatToolCall{ID: item.CallID, Type: "function", Function: openai.ToolCallFunction{Name: name, Arguments: args}}, true
}

func responseCallSummary(item openai.ResponseOutputItem) string {
	summary := "Assistant call: " + responseOutputItemPromptSummary(item)
	if item.CallID != "" {
		summary += " call_id=" + item.CallID
	}
	return summary
}

func responseRecordReasoning(record sessionstore.ResponseRecord) string {
	var parts []string
	for _, item := range record.Output {
		if item.Type != "reasoning" {
			continue
		}
		for _, summary := range item.Summary {
			if summary.Text != "" {
				parts = append(parts, summary.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func storedToolOutputIsFunction(output toolcatalog.StoredToolOutput) bool {
	return output.Type == "" || output.Type == "function_call_output"
}

// responseContinuationFollowUp keeps a hydrated continuation from sending an
// empty prompt. The conversation now lives in the session's replayed events, so
// a request that carried no new input still needs something to send.
func responseContinuationFollowUp(current resolvedPrompt) resolvedPrompt {
	if strings.TrimSpace(current.Text) == "" && len(current.Attachments) == 0 {
		current.Text = "Continue."
	}
	return current
}

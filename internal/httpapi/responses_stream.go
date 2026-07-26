package httpapi

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

type responseStreamWriteResult struct {
	Response       *openai.Response
	Err            error
	WriteFailed    bool
	FailureWritten bool
}

// outputItemIndexer is the single source of truth for `output_index` on a
// Responses stream. An item is assigned an index the first time it is
// announced and keeps it for every later event, so the index can never be
// re-derived from ad-hoc arithmetic about which item type happened to come
// first, and two items can never end up sharing an index.
//
// The gateway builds the terminal response in the same announcement order (see
// copilotgw.orderOutputItems), so an item's streamed output_index also equals
// its position in the persisted record.
type outputItemIndexer struct{ ids []string }

func (x *outputItemIndexer) indexOf(id string) int {
	for i, seen := range x.ids {
		if seen == id {
			return i
		}
	}
	x.ids = append(x.ids, id)
	return len(x.ids) - 1
}

// streamedToolCall is one tool call whose arguments this stream published
// incrementally: the in-progress item the gateway announced it under, the text
// already delivered, and - once the turn is finished - what is still owed.
type streamedToolCall struct {
	item      openai.ResponseOutputItem
	delivered strings.Builder
	remaining string
}

func (s *streamedToolCall) custom() bool { return s.item.Type == "custom_tool_call" }

// deltaEvent names the incremental event this item's input belongs to. A
// freeform custom tool streams raw grammar input under its own event name;
// reporting it as response.function_call_arguments.* would tell the client the
// bytes are JSON arguments when they are not.
func (s *streamedToolCall) deltaEvent() string {
	if s.custom() {
		return "response.custom_tool_call_input.delta"
	}
	return "response.function_call_arguments.delta"
}

// incompleteItem is the item as far as the stream got, for a turn that failed
// after the fragments began. Every item this stream announces has to be closed.
func (s *streamedToolCall) incompleteItem() openai.ResponseOutputItem {
	item := s.item
	item.Status = "incomplete"
	if s.custom() {
		item.Input = s.delivered.String()
	} else {
		item.Arguments = s.delivered.String()
	}
	return item
}

// toolCallItemInput is the value a tool call's streamed fragments accumulate
// towards, which differs by item type.
func toolCallItemInput(item openai.ResponseOutputItem) string {
	if item.Type == "custom_tool_call" {
		return item.Input
	}
	return item.Arguments
}

func writeResponseLifecycleStart(writer responseEventWriter, req copilotgw.ResponseRequest, status string) (*openai.Response, error) {
	var previous *string
	if req.PreviousResponseID != "" {
		previous = &req.PreviousResponseID
	}
	initial := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: responseCreatedAt(req), Status: status, Model: req.Model, Instructions: req.Instructions, Output: []openai.ResponseOutputItem{}, OutputText: "", ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Error: nil, IncompleteDetails: nil}
	if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.created", Response: initial, Status: initial.Status}); err != nil {
		return nil, err
	}
	if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.in_progress", Response: initial, Status: initial.Status}); err != nil {
		return nil, err
	}
	return initial, nil
}

// responseCreatedAt returns the creation time the request was stamped with, so
// every frame the HTTP layer synthesizes for a response agrees with the one the
// gateway built and persisted.
func responseCreatedAt(req copilotgw.ResponseRequest) int64 {
	if req.CreatedAt != 0 {
		return req.CreatedAt
	}
	return openai.UnixNow()
}

func writeWarmResponseEvents(writer responseEventWriter, resp *openai.Response) error {
	writer = newResponseStreamEncoder(writer)
	initial := *resp
	initial.Status = "in_progress"
	initial.Output = []openai.ResponseOutputItem{}
	initial.OutputText = ""
	if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.created", Response: &initial, Status: initial.Status}); err != nil {
		return err
	}
	if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.in_progress", Response: &initial, Status: initial.Status}); err != nil {
		return err
	}
	return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.completed", Response: resp, Status: resp.Status})
}

// writeResponseStreamEvents turns one gateway turn into the Responses event
// sequence every transport serializes. It never constructs a response of its
// own and never renames an output item: the gateway builds the turn's single
// openai.Response, assigns its output-item IDs before the first delta carries
// them here, and persists that same object. This function only decides which
// lifecycle events describe it.
//
// suppressReasoning is the server's reasoning-emission policy. It is a pure
// rendering filter: it drops reasoning from the events and from the terminal
// response copy this function writes, and never touches the gateway's object.
func writeResponseStreamEvents(ctx context.Context, writer responseEventWriter, req copilotgw.ResponseRequest, maxOutputBytes int64, suppressReasoning bool, ch <-chan copilotgw.ResponseStreamEvent) responseStreamWriteResult {
	writer = newResponseStreamEncoder(writer)
	if _, err := writeResponseLifecycleStart(writer, req, "in_progress"); err != nil {
		return responseStreamWriteResult{Err: err, WriteFailed: true}
	}

	var index outputItemIndexer
	// Both item IDs are observed from the gateway, never minted here.
	messageID := ""
	messageStarted := false
	messageDone := false
	contentPartStarted := false
	var messageText strings.Builder
	// Reasoning streaming state. Reasoning summary events are normally emitted
	// ahead of the message item (the SDK completes reasoning before content), so
	// the indexer hands reasoning index 0 and the message index 1.
	reasoningItemID := ""
	reasoningStarted := false
	reasoningSummaryDone := false
	reasoningItemDone := false
	var reasoningText strings.Builder
	// Tool calls whose arguments arrived as fragments, keyed by output-item id,
	// plus the order they were announced in so a failed turn can close them all.
	toolCalls := map[string]*streamedToolCall{}
	var toolCallOrder []string
	toolCallBytes := 0
	if maxOutputBytes <= 0 {
		maxOutputBytes = config.DefaultMaxTurnOutputBytes
	}
	// streamedBytes is everything this stream has accumulated for terminal
	// reconciliation, which is what the output ceiling actually bounds.
	streamedBytes := func() int64 { return int64(reasoningText.Len() + messageText.Len() + toolCallBytes) }
	emitReasoningStart := func() error {
		if reasoningStarted {
			return nil
		}
		reasoningStarted = true
		idx := index.indexOf(reasoningItemID)
		summaryIdx := 0
		item := openai.ResponseOutputItem{ID: reasoningItemID, Type: "reasoning", Status: "in_progress", Summary: []openai.ResponseReasoningSummary{}}
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &item, Status: item.Status}); err != nil {
			return err
		}
		// OpenAI brackets the summary text with a summary_part add/done so SDK
		// stream accumulators have a part[0] to attach the text deltas to.
		return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_part.added", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Part: openai.ResponseReasoningSummary{Type: "summary_text", Text: ""}})
	}
	closeReasoningSummary := func() error {
		if !reasoningStarted || reasoningSummaryDone {
			return nil
		}
		reasoningSummaryDone = true
		idx := index.indexOf(reasoningItemID)
		summaryIdx := 0
		text := reasoningText.String()
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_text.done", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Text: text}); err != nil {
			return err
		}
		return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_part.done", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Part: openai.ResponseReasoningSummary{Type: "summary_text", Text: text}})
	}
	finishReasoningItem := func(resp *openai.Response, status string) error {
		if !reasoningStarted || reasoningItemDone {
			return nil
		}
		if err := closeReasoningSummary(); err != nil {
			return err
		}
		idx := index.indexOf(reasoningItemID)
		text := reasoningText.String()
		terminal, err := terminalOutputItem(resp, reasoningItemID, "reasoning")
		if err != nil {
			return err
		}
		item := openai.ResponseOutputItem{ID: reasoningItemID, Type: "reasoning", Status: status, Summary: []openai.ResponseReasoningSummary{}}
		if terminal != nil {
			item = *terminal
		}
		item.Status = status
		if len(item.Summary) == 0 && text != "" {
			item.Summary = []openai.ResponseReasoningSummary{{Type: "summary_text", Text: text}}
		}
		reasoningItemDone = true
		return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item, Status: item.Status})
	}
	// reconcileStreamedReasoning announces reasoning the turn only resolved at
	// the end, as a suffix of what was already streamed. Summary *parts* are a
	// streaming construct: when the first part was already closed the suffix is
	// bracketed as a second part on the wire, but the terminal item keeps the
	// consolidated summary the gateway built and persisted. Rewriting the
	// terminal item here would make the streamed response and the stored record
	// disagree, since persistence already happened in the gateway.
	reconcileStreamedReasoning := func(resp *openai.Response) error {
		if !reasoningStarted {
			return nil
		}
		item, err := terminalOutputItem(resp, reasoningItemID, "reasoning")
		if err != nil {
			return err
		}
		if item == nil || len(item.Summary) == 0 {
			return nil
		}
		var terminal strings.Builder
		for _, summary := range item.Summary {
			terminal.WriteString(summary.Text)
		}
		finalText := terminal.String()
		streamed := reasoningText.String()
		if finalText == "" {
			return nil
		}
		suffix, err := terminalStreamSuffix(finalText, streamed, "response stream terminal reasoning does not match streamed reasoning")
		if err != nil {
			return err
		}
		if suffix == "" {
			return nil
		}
		if streamedBytes()+int64(len(suffix)) > maxOutputBytes {
			return apierr.Upstream("response output exceeded stream size limit")
		}
		idx := index.indexOf(reasoningItemID)
		summaryIdx := 0
		if reasoningSummaryDone {
			// The first summary part was closed before content began. Represent the
			// terminal-only suffix as a second part on the same reasoning item.
			summaryIdx = 1
			part := openai.ResponseReasoningSummary{Type: "summary_text", Text: ""}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_part.added", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Part: part}); err != nil {
				return err
			}
		}
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Delta: suffix}); err != nil {
			return err
		}
		if reasoningSummaryDone {
			part := openai.ResponseReasoningSummary{Type: "summary_text", Text: suffix}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_text.done", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Text: suffix}); err != nil {
				return err
			}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_part.done", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Part: part}); err != nil {
				return err
			}
		}
		reasoningText.WriteString(suffix)
		return nil
	}

	// reconcileUnstreamedReasoning announces final-only reasoning (including
	// encrypted-only reasoning). If message content already occupied index 0 the
	// indexer hands the late item index 1, which is also where the gateway put it
	// in the terminal output.
	reconcileUnstreamedReasoning := func(resp *openai.Response) error {
		if reasoningStarted {
			return nil
		}
		item, ok := finalReasoningItem(resp)
		if !ok {
			return nil
		}
		reasoningStarted = true
		reasoningSummaryDone = true
		reasoningItemDone = true
		reasoningItemID = item.ID
		idx := index.indexOf(item.ID)
		added := item
		added.Status = "in_progress"
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &added, Status: added.Status}); err != nil {
			return err
		}
		for summaryIndex, summary := range item.Summary {
			si := summaryIndex
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_part.added", OutputIndex: &idx, SummaryIndex: &si, ItemID: item.ID, Part: openai.ResponseReasoningSummary{Type: summary.Type, Text: ""}}); err != nil {
				return err
			}
			if summary.Text != "" {
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: &idx, SummaryIndex: &si, ItemID: item.ID, Delta: summary.Text}); err != nil {
					return err
				}
			}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_text.done", OutputIndex: &idx, SummaryIndex: &si, ItemID: item.ID, Text: summary.Text}); err != nil {
				return err
			}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_part.done", OutputIndex: &idx, SummaryIndex: &si, ItemID: item.ID, Part: summary}); err != nil {
				return err
			}
		}
		done := item
		done.Status = "completed"
		return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &done, Status: done.Status})
	}
	finishIncompleteOutput := func() error {
		// No terminal response exists on this path, so the reasoning item is
		// closed from the streamed text alone.
		if err := finishReasoningItem(nil, "incomplete"); err != nil {
			return err
		}
		// Every item this stream announced has to be closed, including a tool call
		// whose arguments were still arriving when the turn failed.
		for _, itemID := range toolCallOrder {
			idx := index.indexOf(itemID)
			item := toolCalls[itemID].incompleteItem()
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item, Status: item.Status}); err != nil {
				return err
			}
		}
		if !messageStarted || messageDone {
			return nil
		}
		msgIdx := index.indexOf(messageID)
		contentIdx := 0
		text := messageText.String()
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.done", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Text: text}); err != nil {
			return err
		}
		if contentPartStarted {
			part := openai.ResponseText{Type: "output_text", Text: text}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.content_part.done", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Part: &part}); err != nil {
				return err
			}
		}
		messageDone = true
		item := openai.ResponseOutputItem{ID: messageID, Type: "message", Status: "incomplete", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: text}}}
		return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &msgIdx, Item: &item, Status: item.Status})
	}
	// incompleteResponseOutput rebuilds the partial output for a failure frame.
	// It walks the indexer so the items land at the indices they were streamed
	// at, rather than re-deriving an order.
	incompleteResponseOutput := func() ([]openai.ResponseOutputItem, string) {
		partial := map[string]openai.ResponseOutputItem{}
		if reasoningStarted && reasoningItemID != "" {
			item := openai.ResponseOutputItem{ID: reasoningItemID, Type: "reasoning", Status: "incomplete", Summary: []openai.ResponseReasoningSummary{}}
			if text := reasoningText.String(); text != "" {
				item.Summary = []openai.ResponseReasoningSummary{{Type: "summary_text", Text: text}}
			}
			partial[reasoningItemID] = item
		}
		if messageStarted && messageID != "" {
			partial[messageID] = openai.ResponseOutputItem{ID: messageID, Type: "message", Status: "incomplete", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: messageText.String()}}}
		}
		for _, itemID := range toolCallOrder {
			partial[itemID] = toolCalls[itemID].incompleteItem()
		}
		output := make([]openai.ResponseOutputItem, 0, len(partial))
		for _, id := range index.ids {
			if item, ok := partial[id]; ok {
				output = append(output, item)
			}
		}
		return output, messageText.String()
	}
	writeFailure := func(streamErr error) responseStreamWriteResult {
		if err := finishIncompleteOutput(); err != nil {
			return responseStreamWriteResult{Err: err, WriteFailed: true}
		}
		output, outputText := incompleteResponseOutput()
		if err := writeResponseFailedEvent(writer, req, streamErr, output, outputText); err != nil {
			return responseStreamWriteResult{Err: err, WriteFailed: true}
		}
		return responseStreamWriteResult{Err: streamErr, FailureWritten: true}
	}
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				return responseStreamWriteResult{Err: context.Canceled, WriteFailed: true}
			}
			return writeFailure(apierr.Timeout())
		case ev, ok := <-ch:
			if !ok {
				if ctx.Err() == context.Canceled {
					return responseStreamWriteResult{Err: context.Canceled, WriteFailed: true}
				}
				if ctx.Err() == context.DeadlineExceeded {
					return writeFailure(apierr.Timeout())
				}
				return writeFailure(apierr.Upstream("response stream ended before a terminal event"))
			}
			switch ev.Kind {
			case "reasoning_delta":
				if suppressReasoning {
					continue
				}
				if ev.Delta == "" {
					continue
				}
				if ev.ItemID == "" {
					return writeFailure(apierr.Internal("response stream reasoning delta is missing its output item id"))
				}
				reasoningItemID = ev.ItemID
				if err := emitReasoningStart(); err != nil {
					return responseStreamWriteResult{Err: err, WriteFailed: true}
				}
				idx := index.indexOf(reasoningItemID)
				summaryIdx := 0
				if streamedBytes()+int64(len(ev.Delta)) > maxOutputBytes {
					return writeFailure(apierr.Upstream("response output exceeded stream size limit"))
				}
				reasoningText.WriteString(ev.Delta)
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Delta: ev.Delta}); err != nil {
					return responseStreamWriteResult{Err: err, WriteFailed: true}
				}
			case "delta":
				if ev.ItemID == "" {
					return writeFailure(apierr.Internal("response stream delta is missing its output item id"))
				}
				messageID = ev.ItemID
				if err := closeReasoningSummary(); err != nil {
					return responseStreamWriteResult{Err: err, WriteFailed: true}
				}
				if streamedBytes()+int64(len(ev.Delta)) > maxOutputBytes {
					return writeFailure(apierr.Upstream("response output exceeded stream size limit"))
				}
				msgIdx := index.indexOf(messageID)
				contentIdx := 0
				if !messageStarted {
					item := openai.ResponseOutputItem{ID: messageID, Type: "message", Status: "in_progress", Role: "assistant", Content: []openai.ResponseText{}}
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &msgIdx, Item: &item, Status: item.Status}); err != nil {
						return responseStreamWriteResult{Err: err, WriteFailed: true}
					}
					messageStarted = true
				}
				if !contentPartStarted {
					part := openai.ResponseText{Type: "output_text", Text: ""}
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.content_part.added", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Part: &part}); err != nil {
						return responseStreamWriteResult{Err: err, WriteFailed: true}
					}
					contentPartStarted = true
				}
				messageText.WriteString(ev.Delta)
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.delta", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Delta: ev.Delta}); err != nil {
					return responseStreamWriteResult{Err: err, WriteFailed: true}
				}
			case "tool_call_delta":
				// The gateway builds the in-progress item so this layer never has
				// to invent one; without it a fragment could not be announced.
				if ev.ItemID == "" || ev.Item == nil {
					return writeFailure(apierr.Internal("response stream tool-call delta is missing its output item"))
				}
				if ev.Delta == "" {
					continue
				}
				if err := closeReasoningSummary(); err != nil {
					return responseStreamWriteResult{Err: err, WriteFailed: true}
				}
				if streamedBytes()+int64(len(ev.Delta)) > maxOutputBytes {
					return writeFailure(apierr.Upstream("response output exceeded stream size limit"))
				}
				idx := index.indexOf(ev.ItemID)
				streamed, started := toolCalls[ev.ItemID]
				if !started {
					streamed = &streamedToolCall{item: *ev.Item}
					toolCalls[ev.ItemID] = streamed
					toolCallOrder = append(toolCallOrder, ev.ItemID)
					added := streamed.item
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &added, Status: added.Status}); err != nil {
						return responseStreamWriteResult{Err: err, WriteFailed: true}
					}
				}
				streamed.delivered.WriteString(ev.Delta)
				toolCallBytes += len(ev.Delta)
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: streamed.deltaEvent(), OutputIndex: &idx, ItemID: ev.ItemID, Delta: ev.Delta}); err != nil {
					return responseStreamWriteResult{Err: err, WriteFailed: true}
				}
			case "response":
				if ev.Response == nil {
					return writeFailure(apierr.Internal("response stream returned an empty response"))
				}
				// Reasoning-emission filtering is the one presentation concern
				// applied at the edge: it hides reasoning items the operator asked
				// not to emit without changing anything the gateway stored. The
				// gateway memoises one response object per turn, so this must never
				// filter in place - see filterResponseReasoning.
				ev.Response = filterResponseReasoning(ev.Response, suppressReasoning)
				if err := reconcileStreamedReasoning(ev.Response); err != nil {
					return writeFailure(err)
				}
				// Reconciled before any terminal tool-call event is written, so a
				// divergence closes the announced items and fails the response
				// instead of emitting arguments that contradict the fragments.
				if err := reconcileStreamedToolCalls(ev.Response, toolCalls, toolCallOrder); err != nil {
					return writeFailure(err)
				}
				payloadBytes, sizeErr := responseOutputPayloadBytes(ev.Response)
				if sizeErr != nil {
					return writeFailure(apierr.Upstream("failed to measure response output size"))
				}
				if payloadBytes > maxOutputBytes {
					return writeFailure(apierr.Upstream("response output exceeded stream size limit"))
				}
				if err := finishReasoningItem(ev.Response, "completed"); err != nil {
					return writeFailure(err)
				}
				if err := reconcileUnstreamedReasoning(ev.Response); err != nil {
					return responseStreamWriteResult{Err: err, WriteFailed: true}
				}
				if messageStarted {
					item, err := terminalOutputItem(ev.Response, messageID, "message")
					if err != nil {
						return writeFailure(err)
					}
					if item == nil {
						return writeFailure(apierr.Upstream("response stream terminal response is missing the streamed message item"))
					}
					text := messageText.String()
					msgIdx := index.indexOf(messageID)
					contentIdx := 0
					if terminalText := outputItemText(*item); terminalText != text {
						suffix, err := terminalStreamSuffix(terminalText, text, "response stream terminal text does not match streamed content")
						if err != nil {
							return writeFailure(err)
						}
						if streamedBytes()+int64(len(suffix)) > maxOutputBytes {
							return writeFailure(apierr.Upstream("response output exceeded stream size limit"))
						}
						if suffix != "" {
							if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.delta", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Delta: suffix}); err != nil {
								return responseStreamWriteResult{Err: err, WriteFailed: true}
							}
							messageText.WriteString(suffix)
							text = messageText.String()
						}
					}
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.done", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Text: text}); err != nil {
						return responseStreamWriteResult{Err: err, WriteFailed: true}
					}
					if contentPartStarted {
						part := openai.ResponseText{Type: "output_text", Text: text}
						if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.content_part.done", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Part: &part}); err != nil {
							return responseStreamWriteResult{Err: err, WriteFailed: true}
						}
					}
					messageDone = true
					done := *item
					done.Status = "completed"
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &msgIdx, Item: &done, Status: done.Status}); err != nil {
						return responseStreamWriteResult{Err: err, WriteFailed: true}
					}
				} else if err := writeUnstreamedMessageEvents(writer, &index, ev.Response); err != nil {
					return responseStreamWriteResult{Err: err, WriteFailed: true}
				}
				if err := writeResponseOutputEvents(writer, &index, ev.Response, toolCalls); err != nil {
					return responseStreamWriteResult{Err: err, WriteFailed: true}
				}
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.completed", Response: ev.Response, Status: ev.Response.Status}); err != nil {
					return responseStreamWriteResult{Err: err, WriteFailed: true}
				}
				return responseStreamWriteResult{Response: ev.Response}
			case "error":
				return writeFailure(ev.Error)
			}
		}
	}
}

func responseOutputPayloadBytes(resp *openai.Response) (int64, error) {
	if resp == nil {
		return 0, nil
	}
	output, err := json.Marshal(resp.Output)
	if err != nil {
		return 0, err
	}
	return int64(len(output) + len(resp.OutputText)), nil
}

func outputItemText(item openai.ResponseOutputItem) string {
	var text strings.Builder
	for _, content := range item.Content {
		if content.Type == "output_text" {
			text.WriteString(content.Text)
		}
	}
	return text.String()
}

// terminalOutputItem returns the terminal response's item with the given ID.
// The gateway owns output-item identity, so an item the stream already
// announced has to come back under the same ID. A same-typed item under a
// different ID means something other than the gateway minted an ID, which is
// exactly the divergence this layer exists to prevent, so it is reported rather
// than silently rewritten.
func terminalOutputItem(resp *openai.Response, id, itemType string) (*openai.ResponseOutputItem, error) {
	if resp == nil || id == "" {
		return nil, nil
	}
	for i := range resp.Output {
		if resp.Output[i].ID == id {
			return &resp.Output[i], nil
		}
	}
	for i := range resp.Output {
		if resp.Output[i].Type == itemType {
			return nil, apierr.Upstream("response stream terminal " + itemType + " item id does not match the streamed item")
		}
	}
	return nil, nil
}

// finalReasoningItem returns the first reasoning output item in a response, if
// any. Used to reconcile reasoning that was present in the final turn but never
// streamed as summary deltas.
func finalReasoningItem(resp *openai.Response) (openai.ResponseOutputItem, bool) {
	if resp == nil {
		return openai.ResponseOutputItem{}, false
	}
	for _, item := range resp.Output {
		if item.Type == "reasoning" {
			return item, true
		}
	}
	return openai.ResponseOutputItem{}, false
}

// filterResponseReasoning renders resp without its reasoning items. It is
// copy-on-write on purpose: the gateway memoises a single *openai.Response per
// turn and persists that same object, so filtering in place would let a display
// preference destroy stored data. The caller's response is never modified.
func filterResponseReasoning(resp *openai.Response, suppress bool) *openai.Response {
	if !suppress || resp == nil {
		return resp
	}
	filtered := *resp
	filtered.Output = make([]openai.ResponseOutputItem, 0, len(resp.Output))
	for _, item := range resp.Output {
		if item.Type != "reasoning" {
			filtered.Output = append(filtered.Output, item)
		}
	}
	return &filtered
}

func writeUnstreamedMessageEvents(writer responseEventWriter, index *outputItemIndexer, resp *openai.Response) error {
	if resp == nil {
		return nil
	}
	for outputIndex := range resp.Output {
		item := resp.Output[outputIndex]
		if item.Type != "message" {
			continue
		}
		idx := index.indexOf(item.ID)
		added := item
		added.Status = "in_progress"
		added.Content = []openai.ResponseText{}
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &added, Status: added.Status}); err != nil {
			return err
		}
		for contentIndex := range item.Content {
			content := item.Content[contentIndex]
			ci := contentIndex
			part := content
			part.Text = ""
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.content_part.added", OutputIndex: &idx, ContentIndex: &ci, ItemID: item.ID, Part: &part}); err != nil {
				return err
			}
			if content.Text != "" {
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.delta", OutputIndex: &idx, ContentIndex: &ci, ItemID: item.ID, Delta: content.Text}); err != nil {
					return err
				}
			}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.done", OutputIndex: &idx, ContentIndex: &ci, ItemID: item.ID, Text: content.Text}); err != nil {
				return err
			}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.content_part.done", OutputIndex: &idx, ContentIndex: &ci, ItemID: item.ID, Part: &content}); err != nil {
				return err
			}
		}
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item, Status: item.Status}); err != nil {
			return err
		}
	}
	return nil
}

// reconcileStreamedToolCalls proves that what the stream already delivered for
// each tool call is a prefix of what the finished turn reports, and records the
// remainder each item still owes.
//
// A mismatch is fatal rather than papered over. The client has already
// accumulated the fragments, so silently emitting the full arguments again
// would double them and silently dropping them would truncate the call; either
// way the client ends up holding arguments that disagree with the record this
// service persisted.
func reconcileStreamedToolCalls(resp *openai.Response, streams map[string]*streamedToolCall, order []string) error {
	for _, itemID := range order {
		streamed := streams[itemID]
		item, err := terminalOutputItem(resp, itemID, streamed.item.Type)
		if err != nil {
			return err
		}
		if item == nil {
			return apierr.Upstream("response stream terminal response is missing the streamed " + streamed.item.Type + " item")
		}
		suffix, err := terminalStreamSuffix(toolCallItemInput(*item), streamed.delivered.String(), "response stream terminal tool-call arguments do not match the streamed arguments")
		if err != nil {
			return err
		}
		streamed.remaining = suffix
	}
	return nil
}

// writeResponseOutputEvents closes out the turn's tool-call items. An item
// whose arguments were streamed keeps the announcement it already received and
// is only owed whatever the fragments did not deliver; an item that was never
// streamed is announced and delivered whole, exactly as it always has been.
// Either way the terminal events are identical, which is what lets a client
// reconcile the two paths without knowing which one it got.
func writeResponseOutputEvents(writer responseEventWriter, index *outputItemIndexer, resp *openai.Response, streams map[string]*streamedToolCall) error {
	if resp == nil {
		return nil
	}
	for i := range resp.Output {
		item := resp.Output[i]
		switch item.Type {
		case "function_call":
			idx := index.indexOf(item.ID)
			streamed := streams[item.ID]
			if streamed == nil {
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &item, Status: item.Status}); err != nil {
					return err
				}
				if item.Arguments != "" {
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: &idx, ItemID: item.ID, Delta: item.Arguments}); err != nil {
						return err
					}
				}
			} else if streamed.remaining != "" {
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: &idx, ItemID: item.ID, Delta: streamed.remaining}); err != nil {
					return err
				}
			}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: &idx, ItemID: item.ID, Arguments: item.Arguments, Name: item.Name}); err != nil {
				return err
			}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item, Status: item.Status}); err != nil {
				return err
			}
		case "custom_tool_call", "tool_search_call":
			idx := index.indexOf(item.ID)
			streamed := streams[item.ID]
			if streamed == nil {
				added := item
				if added.Status == "completed" {
					added.Status = "in_progress"
				}
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &added, Status: added.Status}); err != nil {
					return err
				}
			} else {
				if streamed.remaining != "" {
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.custom_tool_call_input.delta", OutputIndex: &idx, ItemID: item.ID, Delta: streamed.remaining}); err != nil {
						return err
					}
				}
				// A fragment stream needs a terminator of its own; the item's own
				// done event carries the whole item and arrives after it.
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.custom_tool_call_input.done", OutputIndex: &idx, ItemID: item.ID, Input: item.Input}); err != nil {
					return err
				}
			}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item, Status: item.Status}); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeResponseFailedEvent(writer responseEventWriter, req copilotgw.ResponseRequest, err error, output []openai.ResponseOutputItem, outputText string) error {
	obj := errorObject(err)
	var previous *string
	if req.PreviousResponseID != "" {
		previous = &req.PreviousResponseID
	}
	if output == nil {
		output = []openai.ResponseOutputItem{}
	}
	resp := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: responseCreatedAt(req), Status: "failed", Model: req.Model, Instructions: req.Instructions, Output: output, OutputText: outputText, ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Error: obj, IncompleteDetails: nil}
	return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.failed", Response: resp, Error: &obj, Status: resp.Status})
}

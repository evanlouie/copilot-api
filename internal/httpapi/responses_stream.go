package httpapi

import (
	"context"
	"strings"

	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

type responseEventWriter interface {
	WriteResponseEvent(openai.ResponseStreamEvent) error
}

type sseResponseEventWriter struct {
	writer *openai.SSEWriter
}

func (w sseResponseEventWriter) WriteResponseEvent(ev openai.ResponseStreamEvent) error {
	return w.writer.Event(ev.Type, ev)
}

const maxResponseStreamTextBytes = 100 << 20

type responseStreamWriteResult struct {
	Response    *openai.Response
	Err         error
	WriteFailed bool
}

func writeResponseLifecycleStart(writer responseEventWriter, req copilotgw.ResponseRequest, status string) (*openai.Response, error) {
	var previous *string
	if req.PreviousResponseID != "" {
		previous = &req.PreviousResponseID
	}
	initial := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: status, Model: req.Model, Instructions: req.Instructions, Output: []openai.ResponseOutputItem{}, OutputText: "", ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Error: nil, IncompleteDetails: nil}
	if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.created", Response: initial, Status: initial.Status}); err != nil {
		return nil, err
	}
	if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.in_progress", Response: initial, Status: initial.Status}); err != nil {
		return nil, err
	}
	return initial, nil
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

func writeResponseStreamEvents(ctx context.Context, writer responseEventWriter, req copilotgw.ResponseRequest, ch <-chan copilotgw.ResponseStreamEvent) responseStreamWriteResult {
	writer = newResponseStreamEncoder(writer)
	if _, err := writeResponseLifecycleStart(writer, req, "in_progress"); err != nil {
		return responseStreamWriteResult{Err: err, WriteFailed: true}
	}

	messageID := openai.NewID("msg_")
	messageStarted := false
	messageDone := false
	contentPartStarted := false
	var messageText strings.Builder
	// Reasoning streaming state. Reasoning summary events are emitted ahead of
	// the message item (the SDK completes reasoning before content), so when
	// reasoning is present the message item shifts to output index 1.
	reasoningItemID := ""
	reasoningStarted := false
	reasoningDone := false
	var reasoningText strings.Builder
	messageOutputIndex := 0
	var final *openai.Response
	emitReasoningStart := func() error {
		if reasoningStarted {
			return nil
		}
		reasoningStarted = true
		messageOutputIndex = 1
		idx := 0
		summaryIdx := 0
		item := openai.ResponseOutputItem{ID: reasoningItemID, Type: "reasoning", Status: "in_progress", Summary: []openai.ResponseReasoningSummary{}}
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &item, Status: item.Status}); err != nil {
			return err
		}
		// OpenAI brackets the summary text with a summary_part add/done so SDK
		// stream accumulators have a part[0] to attach the text deltas to.
		return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_part.added", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Part: openai.ResponseReasoningSummary{Type: "summary_text", Text: ""}})
	}
	closeReasoning := func() error {
		if !reasoningStarted || reasoningDone {
			return nil
		}
		reasoningDone = true
		idx := 0
		summaryIdx := 0
		text := reasoningText.String()
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_text.done", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Text: text}); err != nil {
			return err
		}
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_part.done", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Part: openai.ResponseReasoningSummary{Type: "summary_text", Text: text}}); err != nil {
			return err
		}
		item := openai.ResponseOutputItem{ID: reasoningItemID, Type: "reasoning", Status: "completed", Summary: []openai.ResponseReasoningSummary{}}
		if text != "" {
			item.Summary = []openai.ResponseReasoningSummary{{Type: "summary_text", Text: text}}
		}
		return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item, Status: item.Status})
	}
	// reconcileUnstreamedReasoning announces a final reasoning item that produced
	// no streamable summary text (e.g. encrypted-only reasoning). It is only safe
	// before the message item has started; once content has streamed at index 0
	// we cannot retroactively reorder, so the final response.completed stays
	// authoritative.
	reconcileUnstreamedReasoning := func(resp *openai.Response) error {
		if reasoningStarted || messageStarted {
			return nil
		}
		item, ok := finalReasoningItem(resp)
		if !ok {
			return nil
		}
		reasoningStarted = true
		reasoningDone = true
		messageOutputIndex = 1
		idx := 0
		added := item
		added.Status = "in_progress"
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &added, Status: added.Status}); err != nil {
			return err
		}
		done := item
		done.Status = "completed"
		return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &done, Status: done.Status})
	}
	for {
		select {
		case <-ctx.Done():
			return responseStreamWriteResult{Response: final, Err: ctx.Err()}
		case ev, ok := <-ch:
			if !ok {
				return responseStreamWriteResult{Response: final}
			}
			switch ev.Kind {
			case "reasoning_delta":
				if ev.Delta == "" {
					continue
				}
				if reasoningItemID == "" {
					if ev.ReasoningID != "" {
						reasoningItemID = "rs_" + ev.ReasoningID
					} else {
						reasoningItemID = openai.NewID("rs_")
					}
				}
				if err := emitReasoningStart(); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				idx := 0
				summaryIdx := 0
				reasoningText.WriteString(ev.Delta)
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Delta: ev.Delta}); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
			case "delta":
				if err := closeReasoning(); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if messageText.Len()+len(ev.Delta) > maxResponseStreamTextBytes {
					err := openai.Internal("response output exceeded stream text limit")
					if writeErr := writeResponseFailedEvent(writer, req, err); writeErr != nil {
						return responseStreamWriteResult{Response: final, Err: writeErr, WriteFailed: true}
					}
					return responseStreamWriteResult{Response: final, Err: err}
				}
				msgIdx := messageOutputIndex
				contentIdx := 0
				if !messageStarted {
					item := openai.ResponseOutputItem{ID: messageID, Type: "message", Status: "in_progress", Role: "assistant", Content: []openai.ResponseText{}}
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &msgIdx, Item: &item, Status: item.Status}); err != nil {
						return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
					}
					messageStarted = true
				}
				if !contentPartStarted {
					part := openai.ResponseText{Type: "output_text", Text: ""}
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.content_part.added", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Part: &part}); err != nil {
						return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
					}
					contentPartStarted = true
				}
				messageText.WriteString(ev.Delta)
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.delta", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Delta: ev.Delta}); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
			case "response":
				if err := closeReasoning(); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if err := reconcileUnstreamedReasoning(ev.Response); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if messageStarted {
					text := messageText.String()
					msgIdx := messageOutputIndex
					contentIdx := 0
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.done", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Text: text}); err != nil {
						return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
					}
					if contentPartStarted {
						part := openai.ResponseText{Type: "output_text", Text: text}
						if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.content_part.done", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Part: &part}); err != nil {
							return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
						}
					}
					messageDone = true
					if item, idx := streamedMessageItem(ev.Response, messageID, text, messageOutputIndex); item != nil {
						if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: item, Status: item.Status}); err != nil {
							return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
						}
					}
				}
				if err := writeResponseOutputEvents(writer, ev.Response); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.completed", Response: ev.Response, Status: ev.Response.Status}); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				final = ev.Response
			case "error":
				if err := closeReasoning(); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if messageStarted && !messageDone {
					msgIdx := messageOutputIndex
					contentIdx := 0
					text := messageText.String()
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.done", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Text: text}); err != nil {
						return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
					}
					if contentPartStarted {
						part := openai.ResponseText{Type: "output_text", Text: text}
						if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.content_part.done", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Part: &part}); err != nil {
							return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
						}
					}
					item := openai.ResponseOutputItem{ID: messageID, Type: "message", Status: "incomplete", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: text}}}
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &msgIdx, Item: &item, Status: item.Status}); err != nil {
						return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
					}
				}
				if err := writeResponseFailedEvent(writer, req, ev.Error); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				return responseStreamWriteResult{Response: final, Err: ev.Error}
			}
		}
	}
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

func writeResponseOutputEvents(writer responseEventWriter, resp *openai.Response) error {
	if resp == nil {
		return nil
	}
	for i := range resp.Output {
		item := resp.Output[i]
		if item.Type != "function_call" {
			continue
		}
		idx := i
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &item, Status: item.Status}); err != nil {
			return err
		}
		if item.Arguments != "" {
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: &idx, ItemID: item.ID, Delta: item.Arguments}); err != nil {
				return err
			}
		}
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: &idx, ItemID: item.ID, Arguments: item.Arguments, Name: item.Name}); err != nil {
			return err
		}
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item, Status: item.Status}); err != nil {
			return err
		}
	}
	return nil
}

func writeResponseFailedEvent(writer responseEventWriter, req copilotgw.ResponseRequest, err error) error {
	obj := errorObject(err)
	var previous *string
	if req.PreviousResponseID != "" {
		previous = &req.PreviousResponseID
	}
	resp := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "failed", Model: req.Model, Instructions: req.Instructions, Output: []openai.ResponseOutputItem{}, OutputText: "", ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Error: obj, IncompleteDetails: nil}
	return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.failed", Response: resp, Error: &obj, Status: resp.Status})
}

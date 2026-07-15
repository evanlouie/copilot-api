package httpapi

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

type responseEventWriter interface {
	WriteResponseEvent(openai.ResponseStreamEvent) error
}

type sseResponseEventWriter struct {
	server *Server
	ctx    context.Context
	writer *openai.SSEWriter
}

func (w sseResponseEventWriter) WriteResponseEvent(ev openai.ResponseStreamEvent) error {
	if w.server == nil {
		return w.writer.Event(ev.Type, ev)
	}
	var attrs []any
	if w.server.debugEnabled(w.ctx) {
		attrs = responseStreamEventAttrs(ev)
	}
	return w.server.writeSSEEvent(w.ctx, w.writer, ev.Type, ev, attrs...)
}

type responseStreamWriteResult struct {
	Response       *openai.Response
	Err            error
	WriteFailed    bool
	FailureWritten bool
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

func writeResponseStreamEvents(ctx context.Context, writer responseEventWriter, req copilotgw.ResponseRequest, maxOutputBytes int64, ch <-chan copilotgw.ResponseStreamEvent) responseStreamWriteResult {
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
	reasoningSummaryDone := false
	reasoningItemDone := false
	var reasoningText strings.Builder
	messageOutputIndex := 0
	if maxOutputBytes <= 0 {
		maxOutputBytes = config.DefaultMaxTurnOutputBytes
	}
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
	closeReasoningSummary := func() error {
		if !reasoningStarted || reasoningSummaryDone {
			return nil
		}
		reasoningSummaryDone = true
		idx := 0
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
		idx := 0
		text := reasoningText.String()
		item, ok := finalReasoningItem(resp)
		if ok {
			setFinalReasoningItemID(resp, reasoningItemID)
			item.ID = reasoningItemID
		} else {
			item = openai.ResponseOutputItem{ID: reasoningItemID, Type: "reasoning", Status: "completed", Summary: []openai.ResponseReasoningSummary{}}
		}
		item.Status = status
		if len(item.Summary) == 0 && text != "" {
			item.Summary = []openai.ResponseReasoningSummary{{Type: "summary_text", Text: text}}
		}
		reasoningItemDone = true
		return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item, Status: item.Status})
	}
	reconcileStreamedReasoning := func(resp *openai.Response) error {
		if !reasoningStarted {
			return nil
		}
		item, ok := finalReasoningItem(resp)
		if !ok || len(item.Summary) == 0 {
			return nil
		}
		var terminal strings.Builder
		for _, summary := range item.Summary {
			terminal.WriteString(summary.Text)
		}
		finalText := terminal.String()
		streamed := reasoningText.String()
		if finalText == "" || finalText == streamed {
			return nil
		}
		if !strings.HasPrefix(finalText, streamed) {
			return openai.Upstream("response stream terminal reasoning does not match streamed reasoning")
		}
		suffix := strings.TrimPrefix(finalText, streamed)
		if int64(reasoningText.Len()+messageText.Len()+len(suffix)) > maxOutputBytes {
			return openai.Upstream("response output exceeded stream size limit")
		}
		idx := 0
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
			for i := range resp.Output {
				if resp.Output[i].Type == "reasoning" {
					resp.Output[i].Summary = []openai.ResponseReasoningSummary{{Type: "summary_text", Text: streamed}, part}
					break
				}
			}
		}
		reasoningText.WriteString(suffix)
		return nil
	}

	// reconcileUnstreamedReasoning announces final-only reasoning (including
	// encrypted-only reasoning). If message content already occupied index 0,
	// announce the late item at index 1 and keep that ordering in the completed
	// response rather than silently omitting its lifecycle.
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
		idx := 0
		if messageStarted {
			idx = 1
		} else {
			messageOutputIndex = 1
		}
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
		if err := finishReasoningItem(final, "incomplete"); err != nil {
			return err
		}
		if !messageStarted || messageDone {
			return nil
		}
		msgIdx := messageOutputIndex
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
	incompleteResponseOutput := func() ([]openai.ResponseOutputItem, string) {
		var reasoningItem *openai.ResponseOutputItem
		if reasoningStarted {
			item := openai.ResponseOutputItem{ID: reasoningItemID, Type: "reasoning", Status: "incomplete", Summary: []openai.ResponseReasoningSummary{}}
			if text := reasoningText.String(); text != "" {
				item.Summary = []openai.ResponseReasoningSummary{{Type: "summary_text", Text: text}}
			}
			reasoningItem = &item
		}
		var messageItem *openai.ResponseOutputItem
		if messageStarted {
			item := openai.ResponseOutputItem{ID: messageID, Type: "message", Status: "incomplete", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: messageText.String()}}}
			messageItem = &item
		}
		output := make([]openai.ResponseOutputItem, 0, 2)
		if messageItem != nil && messageOutputIndex == 0 {
			output = append(output, *messageItem)
		}
		if reasoningItem != nil {
			output = append(output, *reasoningItem)
		}
		if messageItem != nil && messageOutputIndex != 0 {
			output = append(output, *messageItem)
		}
		return output, messageText.String()
	}
	writeFailure := func(streamErr error) responseStreamWriteResult {
		if err := finishIncompleteOutput(); err != nil {
			return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
		}
		output, outputText := incompleteResponseOutput()
		if err := writeResponseFailedEvent(writer, req, streamErr, output, outputText); err != nil {
			return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
		}
		return responseStreamWriteResult{Response: final, Err: streamErr, FailureWritten: true}
	}
	for {
		select {
		case <-ctx.Done():
			if final != nil {
				return responseStreamWriteResult{Response: final}
			}
			if ctx.Err() == context.Canceled {
				return responseStreamWriteResult{Err: context.Canceled, WriteFailed: true}
			}
			return writeFailure(openai.Timeout())
		case ev, ok := <-ch:
			if !ok {
				if final != nil {
					return responseStreamWriteResult{Response: final}
				}
				if ctx.Err() == context.Canceled {
					return responseStreamWriteResult{Err: context.Canceled, WriteFailed: true}
				}
				if ctx.Err() == context.DeadlineExceeded {
					return writeFailure(openai.Timeout())
				}
				return writeFailure(openai.Upstream("response stream ended before a terminal event"))
			}
			switch ev.Kind {
			case "reasoning_delta":
				if req.SuppressReasoning {
					continue
				}
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
				if int64(reasoningText.Len()+messageText.Len()+len(ev.Delta)) > maxOutputBytes {
					return writeFailure(openai.Upstream("response output exceeded stream size limit"))
				}
				reasoningText.WriteString(ev.Delta)
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: &idx, SummaryIndex: &summaryIdx, ItemID: reasoningItemID, Delta: ev.Delta}); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
			case "delta":
				if err := closeReasoningSummary(); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if int64(reasoningText.Len()+messageText.Len()+len(ev.Delta)) > maxOutputBytes {
					return writeFailure(openai.Upstream("response output exceeded stream size limit"))
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
				if ev.Response == nil {
					return writeFailure(openai.Internal("response stream returned an empty response"))
				}
				ev.Response = filterResponseReasoning(ev.Response, req.SuppressReasoning)
				if err := reconcileStreamedReasoning(ev.Response); err != nil {
					return writeFailure(err)
				}
				payloadBytes, sizeErr := responseOutputPayloadBytes(ev.Response)
				if sizeErr != nil {
					return writeFailure(openai.Upstream("failed to measure response output size"))
				}
				if payloadBytes > maxOutputBytes {
					return writeFailure(openai.Upstream("response output exceeded stream size limit"))
				}
				if err := finishReasoningItem(ev.Response, "completed"); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if err := reconcileUnstreamedReasoning(ev.Response); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if messageStarted {
					text := messageText.String()
					msgIdx := messageOutputIndex
					contentIdx := 0
					if terminalText, ok := finalResponseMessageText(ev.Response); ok && terminalText != text {
						if !strings.HasPrefix(terminalText, text) {
							return writeFailure(openai.Upstream("response stream terminal text does not match streamed content"))
						}
						suffix := strings.TrimPrefix(terminalText, text)
						if int64(reasoningText.Len()+messageText.Len()+len(suffix)) > maxOutputBytes {
							return writeFailure(openai.Upstream("response output exceeded stream size limit"))
						}
						if suffix != "" {
							if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.delta", OutputIndex: &msgIdx, ContentIndex: &contentIdx, ItemID: messageID, Delta: suffix}); err != nil {
								return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
							}
							messageText.WriteString(suffix)
							text = messageText.String()
						}
					}
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
				} else if err := writeUnstreamedMessageEvents(writer, ev.Response); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if err := writeResponseOutputEvents(writer, ev.Response); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.completed", Response: ev.Response, Status: ev.Response.Status}); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				final = ev.Response
				return responseStreamWriteResult{Response: final}
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

func finalResponseMessageText(resp *openai.Response) (string, bool) {
	if resp == nil {
		return "", false
	}
	for _, item := range resp.Output {
		if item.Type != "message" {
			continue
		}
		var text strings.Builder
		for _, content := range item.Content {
			if content.Type == "output_text" {
				text.WriteString(content.Text)
			}
		}
		return text.String(), true
	}
	return "", false
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

func setFinalReasoningItemID(resp *openai.Response, id string) {
	if resp == nil || id == "" {
		return
	}
	for i := range resp.Output {
		if resp.Output[i].Type == "reasoning" {
			resp.Output[i].ID = id
			return
		}
	}
}

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

func writeUnstreamedMessageEvents(writer responseEventWriter, resp *openai.Response) error {
	if resp == nil {
		return nil
	}
	for outputIndex := range resp.Output {
		item := resp.Output[outputIndex]
		if item.Type != "message" {
			continue
		}
		idx := outputIndex
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

func writeResponseOutputEvents(writer responseEventWriter, resp *openai.Response) error {
	if resp == nil {
		return nil
	}
	for i := range resp.Output {
		item := resp.Output[i]
		switch item.Type {
		case "function_call":
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
		case "custom_tool_call", "tool_search_call":
			idx := i
			added := item
			if added.Status == "completed" {
				added.Status = "in_progress"
			}
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &added, Status: added.Status}); err != nil {
				return err
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
	resp := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "failed", Model: req.Model, Instructions: req.Instructions, Output: output, OutputText: outputText, ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Error: obj, IncompleteDetails: nil}
	return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.failed", Response: resp, Error: &obj, Status: resp.Status})
}

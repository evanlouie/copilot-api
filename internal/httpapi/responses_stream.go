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
	if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.created", Response: initial}); err != nil {
		return nil, err
	}
	if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.in_progress", Response: initial}); err != nil {
		return nil, err
	}
	return initial, nil
}

func writeWarmResponseEvents(writer responseEventWriter, resp *openai.Response) error {
	initial := *resp
	initial.Status = "in_progress"
	initial.Output = []openai.ResponseOutputItem{}
	initial.OutputText = ""
	if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.created", Response: &initial}); err != nil {
		return err
	}
	if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.in_progress", Response: &initial}); err != nil {
		return err
	}
	return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.completed", Response: resp})
}

func writeResponseStreamEvents(ctx context.Context, writer responseEventWriter, req copilotgw.ResponseRequest, ch <-chan copilotgw.ResponseStreamEvent) responseStreamWriteResult {
	if _, err := writeResponseLifecycleStart(writer, req, "in_progress"); err != nil {
		return responseStreamWriteResult{Err: err, WriteFailed: true}
	}

	messageID := openai.NewID("msg_")
	messageStarted := false
	messageDone := false
	var messageText strings.Builder
	var final *openai.Response
	for {
		select {
		case <-ctx.Done():
			return responseStreamWriteResult{Response: final, Err: ctx.Err()}
		case ev, ok := <-ch:
			if !ok {
				return responseStreamWriteResult{Response: final}
			}
			switch ev.Kind {
			case "delta":
				zero := 0
				if messageText.Len()+len(ev.Delta) > maxResponseStreamTextBytes {
					err := openai.Internal("response output exceeded stream text limit")
					if writeErr := writeResponseFailedEvent(writer, req, err); writeErr != nil {
						return responseStreamWriteResult{Response: final, Err: writeErr, WriteFailed: true}
					}
					return responseStreamWriteResult{Response: final, Err: err}
				}
				if !messageStarted {
					item := openai.ResponseOutputItem{ID: messageID, Type: "message", Status: "in_progress", Role: "assistant", Content: []openai.ResponseText{}}
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &zero, Item: &item}); err != nil {
						return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
					}
					messageStarted = true
				}
				messageText.WriteString(ev.Delta)
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.delta", OutputIndex: &zero, ContentIndex: &zero, ItemID: messageID, Delta: ev.Delta}); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
			case "response":
				if messageStarted {
					text := messageText.String()
					zero := 0
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.done", OutputIndex: &zero, ContentIndex: &zero, ItemID: messageID, Text: text}); err != nil {
						return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
					}
					messageDone = true
					if item, idx := streamedMessageItem(ev.Response, messageID, text); item != nil {
						if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: item}); err != nil {
							return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
						}
					}
				}
				if err := writeResponseOutputEvents(writer, ev.Response); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.completed", Response: ev.Response}); err != nil {
					return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
				}
				final = ev.Response
			case "error":
				if messageStarted && !messageDone {
					zero := 0
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_text.done", OutputIndex: &zero, ContentIndex: &zero, ItemID: messageID, Text: messageText.String()}); err != nil {
						return responseStreamWriteResult{Response: final, Err: err, WriteFailed: true}
					}
					item := openai.ResponseOutputItem{ID: messageID, Type: "message", Status: "incomplete", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: messageText.String()}}}
					if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &zero, Item: &item}); err != nil {
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
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &item}); err != nil {
			return err
		}
		if item.Arguments != "" {
			if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: &idx, ItemID: item.ID, Delta: item.Arguments}); err != nil {
				return err
			}
		}
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: &idx, ItemID: item.ID, Arguments: item.Arguments}); err != nil {
			return err
		}
		if err := writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item}); err != nil {
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
	return writer.WriteResponseEvent(openai.ResponseStreamEvent{Type: "response.failed", Response: resp, Error: &obj})
}

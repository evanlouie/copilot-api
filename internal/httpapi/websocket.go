package httpapi

import "github.com/evanlouie/copilot-api/internal/openai"

// WebSocketErrorEvent is the error envelope pushed on the Responses WebSocket
// transport. It is a stream event, not an HTTP body, so it carries the
// sequence_number every Responses stream event has and discriminates on `type`.
//
// It deliberately carries the error BOTH flat and nested, because OpenAI's own
// clients disagree about where to look and this proxy has to satisfy both:
//
//   - OpenAI's published ResponseErrorEvent is flat - code, message and param at
//     the top level - and openai-dotnet deserializes it that way.
//   - The live service emits {"error": {...}} instead, which an OpenAI
//     maintainer confirmed in openai-dotnet#881 is the service failing to honour
//     its own contract. openai-python's streaming reads error.message and works
//     only against that nested shape (openai-python#2487).
//
// The AI SDK accepts either, trying the nested schema first, but requires
// sequence_number on both - without it the frame falls through to the union's
// catch-all and is silently discarded as an unknown chunk.
//
// SequenceNumber is not omitempty: zero is a legitimate value for a response's
// first frame, and a client that cannot find the field cannot recognise the
// frame as an error at all.
//
// The nested object additionally preserves the error's `type`
// (invalid_request_error, server_error), which the flat shape has nowhere to
// put. Status is this proxy's own addition, mirroring the HTTP status the same
// failure would produce on REST, which is why this type lives with the HTTP
// mapping. Clients that do not know these fields ignore them.
type WebSocketErrorEvent struct {
	EventID        string             `json:"event_id,omitempty"`
	Type           string             `json:"type"`
	SequenceNumber int64              `json:"sequence_number"`
	Code           string             `json:"code,omitempty"`
	Message        string             `json:"message"`
	Param          string             `json:"param,omitempty"`
	Error          openai.ErrorObject `json:"error"`
	Status         int                `json:"status,omitempty"`
}

// NewWebSocketErrorEvent classifies err and renders it as a WebSocket error
// occupying seq in its response's event sequence.
func NewWebSocketErrorEvent(err error, eventID string, seq int64) WebSocketErrorEvent {
	domain := domainError(err)
	obj := errorObject(err)
	return WebSocketErrorEvent{
		EventID:        eventID,
		Type:           "error",
		SequenceNumber: seq,
		Code:           obj.Code,
		Message:        obj.Message,
		Param:          obj.Param,
		Error:          obj,
		Status:         httpStatus(domain.Kind),
	}
}

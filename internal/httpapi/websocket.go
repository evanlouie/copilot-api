package httpapi

import "github.com/evanlouie/copilot-api/internal/openai"

// WebSocketErrorEvent is the error envelope pushed on the Responses WebSocket
// transport. Status mirrors the HTTP status the same failure would produce on
// the REST transport, which is why this type lives with the HTTP mapping.
type WebSocketErrorEvent struct {
	EventID string             `json:"event_id,omitempty"`
	Type    string             `json:"type"`
	Status  int                `json:"status,omitempty"`
	Error   openai.ErrorObject `json:"error"`
}

// NewWebSocketErrorEvent classifies err and renders it as a WebSocket error.
func NewWebSocketErrorEvent(err error, eventID string) WebSocketErrorEvent {
	domain := domainError(err)
	return WebSocketErrorEvent{EventID: eventID, Type: "error", Status: httpStatus(domain.Kind), Error: errorObject(err)}
}

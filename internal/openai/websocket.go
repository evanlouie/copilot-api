package openai

import (
	"encoding/json"
	"errors"
)

type WebSocketClientEvent struct {
	EventID string `json:"event_id,omitempty"`
	Type    string `json:"type"`
}

type WebSocketErrorEvent struct {
	EventID string      `json:"event_id,omitempty"`
	Type    string      `json:"type"`
	Status  int         `json:"status,omitempty"`
	Error   ErrorObject `json:"error"`
}

func NewWebSocketErrorEvent(err error, eventID string) WebSocketErrorEvent {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = Internal("internal server error")
	}
	return WebSocketErrorEvent{EventID: eventID, Type: "error", Status: apiErr.Status, Error: ErrorObject{Message: apiErr.Message, Type: apiErr.Type, Param: apiErr.Param, Code: apiErr.Code}}
}

type ResponseCreateEvent struct {
	EventID  string          `json:"event_id,omitempty"`
	Type     string          `json:"type"`
	Response json.RawMessage `json:"response,omitempty"`
}

package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

type ErrorObject struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

type ErrorEnvelope struct {
	Error ErrorObject `json:"error"`
}

type APIError struct {
	Status  int
	Message string
	Type    string
	Param   string
	Code    string
}

func (e *APIError) Error() string { return e.Message }

func InvalidRequest(message, param string) *APIError {
	return &APIError{Status: http.StatusBadRequest, Message: message, Type: "invalid_request_error", Param: param}
}

func RequestTooLarge() *APIError {
	return &APIError{Status: http.StatusRequestEntityTooLarge, Message: "request body exceeds the configured size limit", Type: "invalid_request_error", Param: "body", Code: "request_too_large"}
}

func Unauthorized(message string) *APIError {
	return &APIError{Status: http.StatusUnauthorized, Message: message, Type: "invalid_request_error", Code: "invalid_api_key"}
}

func NotFound(message, code string) *APIError {
	return &APIError{Status: http.StatusNotFound, Message: message, Type: "invalid_request_error", Code: code}
}

func PreviousResponseNotFound(id string) *APIError {
	return &APIError{Status: http.StatusBadRequest, Message: fmt.Sprintf("Previous response with id %q not found.", id), Type: "invalid_request_error", Param: "previous_response_id", Code: "previous_response_not_found"}
}

func Upstream(message string) *APIError {
	return &APIError{Status: http.StatusBadGateway, Message: message, Type: "server_error", Code: "upstream_error"}
}

func Timeout() *APIError {
	return &APIError{Status: http.StatusGatewayTimeout, Message: "request timed out", Type: "server_error", Code: "request_timeout"}
}

func Internal(message string) *APIError {
	return &APIError{Status: http.StatusInternalServerError, Message: message, Type: "server_error", Code: "internal_error"}
}

func WriteError(w http.ResponseWriter, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = Internal("internal server error")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(apiErr.Status)
	_ = json.NewEncoder(w).Encode(ErrorEnvelope{Error: ErrorObject{Message: apiErr.Message, Type: apiErr.Type, Param: apiErr.Param, Code: apiErr.Code}})
}

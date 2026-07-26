package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// This file is the single place where the transport-neutral domain error
// taxonomy (internal/apierr) is translated into HTTP. No other package may
// reference an HTTP status code.

// httpStatus maps a domain error onto its HTTP status.
func httpStatus(kind apierr.Kind) int {
	switch kind {
	case apierr.KindInvalidInput:
		return http.StatusBadRequest
	case apierr.KindUnauthorized:
		return http.StatusUnauthorized
	case apierr.KindNotFound:
		return http.StatusNotFound
	case apierr.KindTooLarge:
		return http.StatusRequestEntityTooLarge
	case apierr.KindUpstream:
		return http.StatusBadGateway
	case apierr.KindTimeout:
		return http.StatusGatewayTimeout
	case apierr.KindInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// openAIErrorType maps a domain error onto the OpenAI error-object `type`.
func openAIErrorType(kind apierr.Kind) string {
	switch kind {
	case apierr.KindUpstream, apierr.KindTimeout, apierr.KindInternal:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// domainError recovers the classified domain error, defaulting to an opaque
// internal fault so unclassified failures never leak their message.
func domainError(err error) *apierr.Error {
	var domain *apierr.Error
	if errors.As(err, &domain) {
		return domain
	}
	return apierr.Internal("internal server error")
}

func errorObject(err error) openai.ErrorObject {
	domain := domainError(err)
	return openai.ErrorObject{Message: domain.Message, Type: openAIErrorType(domain.Kind), Param: domain.Param, Code: domain.Code}
}

// WriteError renders err as an OpenAI error envelope with the mapped status.
func WriteError(w http.ResponseWriter, err error) {
	domain := domainError(err)
	writeErrorObject(w, httpStatus(domain.Kind), errorObject(err))
}

// writeErrorObject emits an error envelope with an explicit status, for the few
// failures that are purely transport-level and have no domain counterpart.
func writeErrorObject(w http.ResponseWriter, status int, obj openai.ErrorObject) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openai.ErrorEnvelope{Error: obj})
}

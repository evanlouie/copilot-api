package openai

// ErrorObject is the OpenAI error payload shared by the JSON error envelope,
// the WebSocket error event, and streaming `error` events.
type ErrorObject struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// ErrorEnvelope is the top-level body of a failed OpenAI REST call.
type ErrorEnvelope struct {
	Error ErrorObject `json:"error"`
}

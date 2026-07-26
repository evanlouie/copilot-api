package openai

import (
	"encoding/json"
	"testing"
)

// Explicit JSON null must never count as "the client sent this field".
// openai-python serializes an explicit Python None (which is distinct from its
// NOT_GIVEN sentinel) as `null`, and json.RawMessage captures that literal as a
// 4-byte payload, so `{"stop": null}` has to behave exactly like a request that
// omits "stop" entirely.

// chatFieldSamples pairs every validated chat field with a realistic non-null
// value that must still be rejected once the field is genuinely present.
var chatFieldSamples = map[string]string{
	"audio":                 `{"voice":"alloy","format":"mp3"}`,
	"function_call":         `"auto"`,
	"functions":             `[{"name":"lookup","parameters":{"type":"object"}}]`,
	"logit_bias":            `{"1234":-100}`,
	"logprobs":              `true`,
	"top_logprobs":          `3`,
	"max_tokens":            `20`,
	"max_completion_tokens": `20`,
	"modalities":            `["text"]`,
	"prediction":            `{"type":"content","content":"draft"}`,
	"response_format":       `{"type":"json_object"}`,
	"stop":                  `["done"]`,
	"n":                     `2`,
	"temperature":           `0.5`,
	"top_p":                 `0.9`,
	"presence_penalty":      `0.1`,
	"frequency_penalty":     `0.2`,
	"seed":                  `7`,
	"metadata":              `{"trace":"abc"}`,
	"service_tier":          `"auto"`,
	"user":                  `"user-1"`,
}

// responsesFieldSamples is the Responses equivalent of chatFieldSamples. Values
// for strict-only fields are deliberately well-formed so that strict mode is
// rejecting on presence rather than on content.
var responsesFieldSamples = map[string]string{
	"background":        `true`,
	"max_output_tokens": `256`,
	"truncation":        `"auto"`,
	"temperature":       `0.5`,
	"top_p":             `0.9`,
	"include":           `["reasoning.encrypted_content"]`,
	"reasoning":         `{"effort":"medium"}`,
	"text":              `{"verbosity":"low"}`,
	"metadata":          `{"trace":"abc"}`,
	"service_tier":      `"auto"`,
	"user":              `"user-1"`,
}

func TestRawFieldPresentTreatsNullAsAbsent(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{raw: ``, want: false},
		{raw: `null`, want: false},
		{raw: "  null\n", want: false},
		{raw: `"null"`, want: true},
		{raw: `false`, want: true},
		{raw: `0`, want: true},
		{raw: `""`, want: true},
		{raw: `[]`, want: true},
		{raw: `[null]`, want: true},
		{raw: `{"a":null}`, want: true},
	}
	for _, tt := range tests {
		if got := rawFieldPresent(json.RawMessage(tt.raw)); got != tt.want {
			t.Fatalf("rawFieldPresent(%q) = %t, want %t", tt.raw, got, tt.want)
		}
	}
}

func TestChatNullFieldsAreTreatedAsAbsent(t *testing.T) {
	requireSampleCoverage(t, chatFieldSamples, alwaysRejectChatFields, strictOnlyChatFields)
	for _, field := range alwaysRejectChatFields {
		t.Run("always_reject/"+field.name, func(t *testing.T) {
			req := decodeChatRequest(t, chatBody(field.name, `null`))
			if _, present := req.Raw[field.name]; present {
				t.Fatalf("explicit null left %q in the presence map: %#v", field.name, req.Raw)
			}
			assertChatAccepted(t, req, false)
			assertChatAccepted(t, req, true)

			value := decodeChatRequest(t, chatBody(field.name, chatFieldSamples[field.name]))
			assertChatRejected(t, value, false, field.name)
			assertChatRejected(t, value, true, field.name)
		})
	}
	for _, field := range strictOnlyChatFields {
		t.Run("strict_only/"+field.name, func(t *testing.T) {
			req := decodeChatRequest(t, chatBody(field.name, `null`))
			if _, present := req.Raw[field.name]; present {
				t.Fatalf("explicit null left %q in the presence map: %#v", field.name, req.Raw)
			}
			assertChatAccepted(t, req, false)
			assertChatAccepted(t, req, true)

			value := decodeChatRequest(t, chatBody(field.name, chatFieldSamples[field.name]))
			assertChatAccepted(t, value, false)
			assertChatRejected(t, value, true, field.name)
		})
	}
}

func TestResponsesNullFieldsAreTreatedAsAbsent(t *testing.T) {
	requireSampleCoverage(t, responsesFieldSamples, alwaysRejectResponseFields, strictOnlyResponseFields)
	for _, field := range alwaysRejectResponseFields {
		t.Run("always_reject/"+field.name, func(t *testing.T) {
			req := decodeResponsesRequest(t, responsesBody(field.name, `null`))
			if _, present := req.Raw[field.name]; present {
				t.Fatalf("explicit null left %q in the presence map: %#v", field.name, req.Raw)
			}
			assertResponsesAccepted(t, req, false)
			assertResponsesAccepted(t, req, true)

			value := decodeResponsesRequest(t, responsesBody(field.name, responsesFieldSamples[field.name]))
			assertResponsesRejected(t, value, false, field.name)
			assertResponsesRejected(t, value, true, field.name)
		})
	}
	for _, field := range strictOnlyResponseFields {
		t.Run("strict_only/"+field.name, func(t *testing.T) {
			req := decodeResponsesRequest(t, responsesBody(field.name, `null`))
			if _, present := req.Raw[field.name]; present {
				t.Fatalf("explicit null left %q in the presence map: %#v", field.name, req.Raw)
			}
			assertResponsesAccepted(t, req, false)
			assertResponsesAccepted(t, req, true)

			value := decodeResponsesRequest(t, responsesBody(field.name, responsesFieldSamples[field.name]))
			assertResponsesAccepted(t, value, false)
			assertResponsesRejected(t, value, true, field.name)
		})
	}
}

// TestChatNullFieldsFromOpenAIPythonExplicitNone is the reported reproduction:
// openai-python turns an explicit `stop=None` into `"stop": null` on the wire.
func TestChatNullFieldsFromOpenAIPythonExplicitNone(t *testing.T) {
	req := decodeChatRequest(t, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stop":null,"max_tokens":null,"response_format":null,"logit_bias":null,"n":null,"temperature":null,"user":null}`)
	if len(req.Raw) != 0 {
		t.Fatalf("all-null request produced presence entries: %#v", req.Raw)
	}
	if req.MaxTokens != nil || req.Temperature != nil {
		t.Fatalf("null typed fields decoded to non-nil: MaxTokens=%v Temperature=%v", req.MaxTokens, req.Temperature)
	}
	assertChatAccepted(t, req, false)
	assertChatAccepted(t, req, true)
}

// TestChatNullNNeverReachesIsOne covers the {"n": null} sub-case: isOne cannot
// parse `null` into a json.Number, so presence filtering (not the allow hook) is
// what has to keep the request out of the 400 path.
func TestChatNullNNeverReachesIsOne(t *testing.T) {
	if isOne(json.RawMessage(`null`)) {
		t.Fatal("isOne(null) = true; the null sub-case must be handled by presence filtering")
	}
	assertChatAccepted(t, decodeChatRequest(t, chatBody("n", `null`)), false)
	assertChatAccepted(t, decodeChatRequest(t, chatBody("n", `1`)), false)
	assertChatRejected(t, decodeChatRequest(t, chatBody("n", `2`)), false, "n")
}

// TestNullPresenceDerivedBooleansAreUnset guards the presence-derived booleans
// that internal/httpapi/responses.go builds from decoded requests: an explicit
// null must read as "not set", not as "set to null".
func TestNullPresenceDerivedBooleansAreUnset(t *testing.T) {
	req := decodeResponsesRequest(t, `{"model":"gpt-5","input":"hi","stream":null,"store":null,"tools":null,"tool_choice":null,"parallel_tool_calls":null}`)
	if _, toolsSet := req.Raw["tools"]; toolsSet {
		t.Fatalf(`{"tools":null} marked tools as set: %#v`, req.Raw)
	}
	if req.Store != nil {
		t.Fatalf(`{"store":null} decoded to %v, want nil so StoreSet stays false`, *req.Store)
	}
	if req.Stream {
		t.Fatal(`{"stream":null} decoded to true, want false`)
	}
	if req.Tools != nil {
		t.Fatalf(`{"tools":null} decoded to %#v, want nil`, req.Tools)
	}
	if req.ParallelToolCalls != nil {
		t.Fatalf(`{"parallel_tool_calls":null} decoded to %v, want nil`, *req.ParallelToolCalls)
	}
	assertResponsesAccepted(t, req, false)
	assertResponsesAccepted(t, req, true)

	// An empty array is still an explicit "no tools" and must stay distinguishable.
	set := decodeResponsesRequest(t, `{"model":"gpt-5","input":"hi","tools":[],"store":false}`)
	if _, toolsSet := set.Raw["tools"]; !toolsSet {
		t.Fatalf(`{"tools":[]} must mark tools as set: %#v`, set.Raw)
	}
	if set.Store == nil || *set.Store {
		t.Fatalf(`{"store":false} decoded to %v, want an explicit false`, set.Store)
	}
}

// TestResponsesFieldDecoderMatchesJSONDecoderOnNull keeps the WebSocket
// response.create path (ResponsesRequestFromFields) aligned with the HTTP path.
func TestResponsesFieldDecoderMatchesJSONDecoderOnNull(t *testing.T) {
	data := []byte(`{"model":"gpt-5","input":"hi","tools":null,"store":null,"temperature":null,"reasoning":null,"max_output_tokens":null}`)
	var fromJSON ResponsesRequest
	if err := json.Unmarshal(data, &fromJSON); err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fromFields, err := ResponsesRequestFromFields(fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromJSON.Raw) != 0 || len(fromFields.Raw) != 0 {
		t.Fatalf("null-only request produced presence entries: json %#v fields %#v", fromJSON.Raw, fromFields.Raw)
	}
	assertResponsesAccepted(t, &fromFields, true)
}

func chatBody(field, value string) string {
	return `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"` + field + `":` + value + `}`
}

func responsesBody(field, value string) string {
	return `{"model":"gpt-5","input":"hi","` + field + `":` + value + `}`
}

func decodeChatRequest(t *testing.T, body string) *ChatCompletionRequest {
	t.Helper()
	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return &req
}

func decodeResponsesRequest(t *testing.T, body string) *ResponsesRequest {
	t.Helper()
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return &req
}

func assertChatAccepted(t *testing.T, req *ChatCompletionRequest, strict bool) {
	t.Helper()
	if err := ValidateChatRequest(req, strict); err != nil {
		t.Fatalf("ValidateChatRequest(strict=%t) = %v, want accepted", strict, err)
	}
}

func assertChatRejected(t *testing.T, req *ChatCompletionRequest, strict bool, param string) {
	t.Helper()
	assertRejectedOnParam(t, ValidateChatRequest(req, strict), strict, param)
}

func assertResponsesAccepted(t *testing.T, req *ResponsesRequest, strict bool) {
	t.Helper()
	if err := ValidateResponsesRequest(req, strict); err != nil {
		t.Fatalf("ValidateResponsesRequest(strict=%t) = %v, want accepted", strict, err)
	}
}

func assertResponsesRejected(t *testing.T, req *ResponsesRequest, strict bool, param string) {
	t.Helper()
	assertRejectedOnParam(t, ValidateResponsesRequest(req, strict), strict, param)
}

func assertRejectedOnParam(t *testing.T, err error, strict bool, param string) {
	t.Helper()
	if err == nil {
		t.Fatalf("validation(strict=%t) succeeded, want rejection on %q", strict, param)
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Type != "invalid_request_error" || apiErr.Param != param {
		t.Fatalf("validation(strict=%t) error = %#v, want invalid_request_error on %q", strict, err, param)
	}
}

// requireSampleCoverage fails when a validation table gains or loses a field
// without the null-handling samples above being updated to match.
func requireSampleCoverage(t *testing.T, samples map[string]string, tables ...[]unsupportedField) {
	t.Helper()
	covered := map[string]struct{}{}
	for _, table := range tables {
		for _, field := range table {
			if _, ok := samples[field.name]; !ok {
				t.Fatalf("validated field %q has no non-null sample; add one so its null handling stays covered", field.name)
			}
			covered[field.name] = struct{}{}
		}
	}
	for name := range samples {
		if _, ok := covered[name]; !ok {
			t.Fatalf("sample %q no longer matches any validated field", name)
		}
	}
}

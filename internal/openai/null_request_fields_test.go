package openai

import (
	"encoding/json"
	"testing"

	"github.com/evanlouie/copilot-api/internal/apierr"
)

// Explicit JSON null must never count as "the client sent this field".
// openai-python serializes an explicit Python None (which is distinct from its
// NOT_GIVEN sentinel) as `null`, and json.RawMessage captures that literal as a
// 4-byte payload, so `{"stop": null}` has to behave exactly like a request that
// omits "stop" entirely.

// chatFieldSamples pairs every validated chat field with a realistic non-null
// value that must still be rejected once the field is genuinely present.
var chatFieldSamples = map[string]string{
	"audio":           `{"voice":"alloy","format":"mp3"}`,
	"function_call":   `"auto"`,
	"functions":       `[{"name":"lookup","parameters":{"type":"object"}}]`,
	"logit_bias":      `{"1234":-100}`,
	"modalities":      `["text"]`,
	"prediction":      `{"type":"content","content":"draft"}`,
	"response_format": `{"type":"json_object"}`,
	"stop":            `["done"]`,
	"n":               `2`,
}

// chatIgnoredFieldSamples is the counterpart for chat fields this proxy accepts
// and cannot honour. They are never rejected, so what needs guarding is that an
// explicit null still reads as absent rather than as a value.
var chatIgnoredFieldSamples = map[string]string{
	"logprobs":          `true`,
	"top_logprobs":      `3`,
	"temperature":       `0.5`,
	"top_p":             `0.9`,
	"presence_penalty":  `0.1`,
	"frequency_penalty": `0.2`,
	"seed":              `7`,
	"metadata":          `{"trace":"abc"}`,
	"service_tier":      `"auto"`,
	"user":              `"user-1"`,
}

// responsesFieldSamples is the Responses equivalent of chatFieldSamples.
var responsesFieldSamples = map[string]string{
	"background": `true`,
	"truncation": `"auto"`,
}

// responsesIgnoredFieldSamples is the Responses equivalent of
// chatIgnoredFieldSamples.
var responsesIgnoredFieldSamples = map[string]string{
	"temperature":  `0.5`,
	"top_p":        `0.9`,
	"include":      `["reasoning.encrypted_content"]`,
	"reasoning":    `{"effort":"medium"}`,
	"text":         `{"verbosity":"low"}`,
	"metadata":     `{"trace":"abc"}`,
	"service_tier": `"auto"`,
	"user":         `"user-1"`,
}

func TestRawFieldPresentTreatsNullAsAbsent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	requireSampleCoverage(t, chatFieldSamples, alwaysRejectChatFields)
	for _, field := range alwaysRejectChatFields {
		t.Run("always_reject/"+field.name, func(t *testing.T) {
			t.Parallel()
			req := decodeChatRequest(t, chatBody(field.name, `null`))
			if _, present := req.Raw[field.name]; present {
				t.Fatalf("explicit null left %q in the presence map: %#v", field.name, req.Raw)
			}
			assertChatAccepted(t, req)

			value := decodeChatRequest(t, chatBody(field.name, chatFieldSamples[field.name]))
			assertChatRejected(t, value, field.name)
		})
	}
	for name, sample := range chatIgnoredFieldSamples {
		t.Run("ignored/"+name, func(t *testing.T) {
			t.Parallel()
			req := decodeChatRequest(t, chatBody(name, `null`))
			if _, present := req.Raw[name]; present {
				t.Fatalf("explicit null left %q in the presence map: %#v", name, req.Raw)
			}
			assertChatAccepted(t, req)
			assertChatAccepted(t, decodeChatRequest(t, chatBody(name, sample)))
		})
	}
}

func TestResponsesNullFieldsAreTreatedAsAbsent(t *testing.T) {
	t.Parallel()
	requireSampleCoverage(t, responsesFieldSamples, alwaysRejectResponseFields)
	for _, field := range alwaysRejectResponseFields {
		t.Run("always_reject/"+field.name, func(t *testing.T) {
			t.Parallel()
			req := decodeResponsesRequest(t, responsesBody(field.name, `null`))
			if _, present := req.Raw[field.name]; present {
				t.Fatalf("explicit null left %q in the presence map: %#v", field.name, req.Raw)
			}
			assertResponsesAccepted(t, req)

			value := decodeResponsesRequest(t, responsesBody(field.name, responsesFieldSamples[field.name]))
			assertResponsesRejected(t, value, field.name)
		})
	}
	for name, sample := range responsesIgnoredFieldSamples {
		t.Run("ignored/"+name, func(t *testing.T) {
			t.Parallel()
			req := decodeResponsesRequest(t, responsesBody(name, `null`))
			if _, present := req.Raw[name]; present {
				t.Fatalf("explicit null left %q in the presence map: %#v", name, req.Raw)
			}
			assertResponsesAccepted(t, req)
			assertResponsesAccepted(t, decodeResponsesRequest(t, responsesBody(name, sample)))
		})
	}
}

// TestChatNullFieldsFromOpenAIPythonExplicitNone is the reported reproduction:
// openai-python turns an explicit `stop=None` into `"stop": null` on the wire.
func TestChatNullFieldsFromOpenAIPythonExplicitNone(t *testing.T) {
	t.Parallel()
	req := decodeChatRequest(t, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stop":null,"max_tokens":null,"response_format":null,"logit_bias":null,"n":null,"temperature":null,"user":null}`)
	if len(req.Raw) != 0 {
		t.Fatalf("all-null request produced presence entries: %#v", req.Raw)
	}
	if req.MaxTokens != nil || req.Temperature != nil {
		t.Fatalf("null typed fields decoded to non-nil: MaxTokens=%v Temperature=%v", req.MaxTokens, req.Temperature)
	}
	if _, ok := req.RequestedMaxOutputTokens(); ok {
		t.Fatal(`{"max_tokens":null} reported a requested output cap`)
	}
	assertChatAccepted(t, req)
}

// max_tokens and max_completion_tokens are accepted rather than rejected, so
// their decoded values have to remain reachable for diagnostics.
func TestRequestedMaxOutputTokensPrefersMaxCompletionTokens(t *testing.T) {
	t.Parallel()
	req := decodeChatRequest(t, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"max_completion_tokens":32}`)
	if tokens, ok := req.RequestedMaxOutputTokens(); !ok || tokens != 32 {
		t.Fatalf("RequestedMaxOutputTokens = %d, %t; want 32, true", tokens, ok)
	}
	legacy := decodeChatRequest(t, chatBody("max_tokens", `16`))
	if tokens, ok := legacy.RequestedMaxOutputTokens(); !ok || tokens != 16 {
		t.Fatalf("RequestedMaxOutputTokens = %d, %t; want 16, true", tokens, ok)
	}
	responses := decodeResponsesRequest(t, responsesBody("max_output_tokens", `64`))
	if tokens, ok := ResponsesMaxOutputTokens(responses); !ok || tokens != 64 {
		t.Fatalf("ResponsesMaxOutputTokens = %d, %t; want 64, true", tokens, ok)
	}
	if _, ok := ResponsesMaxOutputTokens(decodeResponsesRequest(t, responsesBody("max_output_tokens", `null`))); ok {
		t.Fatal(`{"max_output_tokens":null} reported a requested output cap`)
	}
}

// TestChatNullNNeverReachesIsOne covers the {"n": null} sub-case: isOne cannot
// parse `null` into a json.Number, so presence filtering (not the allow hook) is
// what has to keep the request out of the 400 path.
func TestChatNullNNeverReachesIsOne(t *testing.T) {
	t.Parallel()
	if isOne(json.RawMessage(`null`)) {
		t.Fatal("isOne(null) = true; the null sub-case must be handled by presence filtering")
	}
	assertChatAccepted(t, decodeChatRequest(t, chatBody("n", `null`)))
	assertChatAccepted(t, decodeChatRequest(t, chatBody("n", `1`)))
	assertChatRejected(t, decodeChatRequest(t, chatBody("n", `2`)), "n")
}

// TestNullPresenceDerivedBooleansAreUnset guards the presence-derived booleans
// that internal/httpapi/responses.go builds from decoded requests: an explicit
// null must read as "not set", not as "set to null".
func TestNullPresenceDerivedBooleansAreUnset(t *testing.T) {
	t.Parallel()
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
	assertResponsesAccepted(t, req)

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
	t.Parallel()
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
	assertResponsesAccepted(t, &fromFields)
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

func assertChatAccepted(t *testing.T, req *ChatCompletionRequest) {
	t.Helper()
	if err := ValidateChatRequest(req); err != nil {
		t.Fatalf("ValidateChatRequest() = %v, want accepted", err)
	}
}

func assertChatRejected(t *testing.T, req *ChatCompletionRequest, param string) {
	t.Helper()
	assertRejectedOnParam(t, ValidateChatRequest(req), param)
}

func assertResponsesAccepted(t *testing.T, req *ResponsesRequest) {
	t.Helper()
	if err := ValidateResponsesRequest(req); err != nil {
		t.Fatalf("ValidateResponsesRequest() = %v, want accepted", err)
	}
}

func assertResponsesRejected(t *testing.T, req *ResponsesRequest, param string) {
	t.Helper()
	assertRejectedOnParam(t, ValidateResponsesRequest(req), param)
}

func assertRejectedOnParam(t *testing.T, err error, param string) {
	t.Helper()
	if err == nil {
		t.Fatalf("validation succeeded, want rejection on %q", param)
	}
	apiErr, ok := err.(*apierr.Error)
	if !ok || apiErr.Kind != apierr.KindInvalidInput || apiErr.Param != param {
		t.Fatalf("validation error = %#v, want invalid_request_error on %q", err, param)
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

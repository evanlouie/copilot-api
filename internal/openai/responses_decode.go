package openai

import "encoding/json"

// ResponsesRequestFromFields decodes a request from already-tokenized object
// fields. WebSocket response.create uses it to merge flat/nested payloads
// without re-marshalling and re-parsing a potentially very large frame.
func ResponsesRequestFromFields(fields map[string]json.RawMessage) (ResponsesRequest, error) {
	var request ResponsesRequest
	request.Raw = map[string]json.RawMessage{}
	decode := func(name string, target any) error {
		raw, ok := fields[name]
		if !ok || len(raw) == 0 {
			return nil
		}
		return json.Unmarshal(raw, target)
	}
	if err := decode("model", &request.Model); err != nil {
		return ResponsesRequest{}, err
	}
	if raw, ok := fields["input"]; ok {
		request.Input = raw
	}
	if err := decode("instructions", &request.Instructions); err != nil {
		return ResponsesRequest{}, err
	}
	if err := decode("previous_response_id", &request.PreviousResponseID); err != nil {
		return ResponsesRequest{}, err
	}
	if err := decode("stream", &request.Stream); err != nil {
		return ResponsesRequest{}, err
	}
	if err := decode("tools", &request.Tools); err != nil {
		return ResponsesRequest{}, err
	}
	if raw, ok := fields["tool_choice"]; ok {
		request.ToolChoice = raw
	}
	if err := decode("parallel_tool_calls", &request.ParallelToolCalls); err != nil {
		return ResponsesRequest{}, err
	}
	if err := decode("store", &request.Store); err != nil {
		return ResponsesRequest{}, err
	}
	if err := decode("reasoning_effort", &request.ReasoningEffort); err != nil {
		return ResponsesRequest{}, err
	}
	for name, target := range map[string]*json.RawMessage{
		"include": &request.Include, "reasoning": &request.Reasoning, "text": &request.Text,
	} {
		if raw, ok := fields[name]; ok {
			*target = raw
		}
	}
	for _, name := range []string{"background", "max_output_tokens", "truncation", "temperature", "top_p", "include", "reasoning", "text", "metadata", "service_tier", "user"} {
		if raw, ok := fields[name]; ok {
			request.Raw[name] = raw
		}
	}
	if _, ok := fields["tools"]; ok {
		request.Raw["tools"] = json.RawMessage(`true`)
	}
	return request, nil
}

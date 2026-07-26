package openai

import (
	"bytes"
	"encoding/json"
)

// Responses API wire DTOs.

type ResponsesRequest struct {
	Model              string                     `json:"model"`
	Input              json.RawMessage            `json:"input"`
	Instructions       string                     `json:"instructions,omitempty"`
	PreviousResponseID string                     `json:"previous_response_id,omitempty"`
	Stream             bool                       `json:"stream,omitempty"`
	Tools              []Tool                     `json:"tools,omitempty"`
	ToolChoice         json.RawMessage            `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                      `json:"parallel_tool_calls,omitempty"`
	Store              *bool                      `json:"store,omitempty"`
	ReasoningEffort    string                     `json:"reasoning_effort,omitempty"`
	Include            json.RawMessage            `json:"include,omitempty"`
	Reasoning          json.RawMessage            `json:"reasoning,omitempty"`
	Metadata           map[string]string          `json:"metadata,omitempty"`
	Text               json.RawMessage            `json:"text,omitempty"`
	Raw                map[string]json.RawMessage `json:"-"`
}

func (r *ResponsesRequest) UnmarshalJSON(data []byte) error {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	decoded, err := ResponsesRequestFromFields(fields)
	if err != nil {
		return err
	}
	*r = decoded
	return nil
}

// rawFieldPresent reports whether a decoded JSON value counts as a field the
// client actually supplied. An explicit JSON null is treated as absent: SDKs
// such as openai-python serialize an explicit Python None (distinct from their
// NOT_GIVEN sentinel) as `null`, and json.RawMessage captures that literal as a
// 4-byte payload, so `null` must not trip the unsupported-field tables.
//
// The comparison stays at the byte level because these payloads can be large
// and string(raw) would copy them.
func rawFieldPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func presentRawFields(fields map[string]json.RawMessage) map[string]json.RawMessage {
	for name, raw := range fields {
		if !rawFieldPresent(raw) {
			delete(fields, name)
		}
	}
	return fields
}

type Response struct {
	ID                 string               `json:"id"`
	Object             string               `json:"object"`
	CreatedAt          int64                `json:"created_at"`
	Status             string               `json:"status"`
	Model              string               `json:"model"`
	Instructions       string               `json:"instructions,omitempty"`
	Output             []ResponseOutputItem `json:"output"`
	OutputText         string               `json:"output_text"`
	ParallelToolCalls  bool                 `json:"parallel_tool_calls"`
	PreviousResponseID *string              `json:"previous_response_id"`
	Store              bool                 `json:"store"`
	Usage              *ResponseUsage       `json:"usage,omitempty"`
	Error              any                  `json:"error"`
	IncompleteDetails  any                  `json:"incomplete_details"`
	Metadata           map[string]string    `json:"metadata,omitempty"`
}

type ResponseOutputItem struct {
	ID               string                     `json:"id,omitempty"`
	Type             string                     `json:"type"`
	Status           string                     `json:"status,omitempty"`
	Role             string                     `json:"role,omitempty"`
	Content          []ResponseText             `json:"content,omitempty"`
	Summary          []ResponseReasoningSummary `json:"summary,omitempty"`
	EncryptedContent string                     `json:"encrypted_content,omitempty"`
	CallID           string                     `json:"call_id,omitempty"`
	Name             string                     `json:"name,omitempty"`
	Namespace        string                     `json:"namespace,omitempty"`
	Arguments        string                     `json:"arguments,omitempty"`
	ArgumentsJSON    json.RawMessage            `json:"-"`
	Input            string                     `json:"input,omitempty"`
	Execution        string                     `json:"execution,omitempty"`
	Output           json.RawMessage            `json:"output,omitempty"`
}

// MarshalJSON emits the fields the Responses schema declares required and
// non-nullable for the item's own type, even when their Go zero value would
// otherwise trip `omitempty`. Shadowing the embedded alias fields drops
// omitempty for the duration of one variant while keeping every other field in
// sync automatically, the same trick ResponseText and ChatCompletionChunk use.
//
// This matters most for the streaming path: `response.output_item.added` opens
// a message with an empty content array and a reasoning item with an empty
// summary array. SDK stream accumulators (openai-python's ResponseStreamState,
// and the Agents SDK on top of it) snapshot that item and then append into
// `item.content` / `item.summary` on the first `response.content_part.added` or
// summary part. A dropped key deserializes to None and the append raises, so
// the arrays have to be on the wire as `[]`.
//
// Fields are only forced for the item types that actually declare them:
// emitting `"content": null` on a function_call would be its own
// compatibility problem.
func (i ResponseOutputItem) MarshalJSON() ([]byte, error) {
	type alias ResponseOutputItem
	switch i.Type {
	case "message":
		// ResponseOutputMessage: id, content, role, status, type.
		content := i.Content
		if content == nil {
			content = []ResponseText{}
		}
		return json.Marshal(struct {
			alias
			ID      string         `json:"id"`
			Status  string         `json:"status"`
			Role    string         `json:"role"`
			Content []ResponseText `json:"content"`
		}{alias: alias(i), ID: i.ID, Status: i.Status, Role: i.Role, Content: content})
	case "reasoning":
		// ResponseReasoningItem: id, summary, type. status and
		// encrypted_content stay optional.
		summary := i.Summary
		if summary == nil {
			summary = []ResponseReasoningSummary{}
		}
		return json.Marshal(struct {
			alias
			ID      string                     `json:"id"`
			Summary []ResponseReasoningSummary `json:"summary"`
		}{alias: alias(i), ID: i.ID, Summary: summary})
	case "function_call":
		// ResponseFunctionToolCall: arguments, call_id, name, type. id,
		// namespace and status stay optional.
		return json.Marshal(struct {
			alias
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{alias: alias(i), CallID: i.CallID, Name: i.Name, Arguments: i.Arguments})
	case "custom_tool_call":
		// ResponseCustomToolCall: call_id, input, name, type. id and namespace
		// stay optional.
		return json.Marshal(struct {
			alias
			CallID string `json:"call_id"`
			Name   string `json:"name"`
			Input  string `json:"input"`
		}{alias: alias(i), CallID: i.CallID, Name: i.Name, Input: i.Input})
	case "tool_search_call":
		// tool_search_call is not part of the published Responses schema, so no
		// field is forced; only the raw-object arguments override applies.
		if len(i.ArgumentsJSON) > 0 {
			return json.Marshal(struct {
				alias
				Arguments json.RawMessage `json:"arguments,omitempty"`
			}{alias: alias(i), Arguments: i.ArgumentsJSON})
		}
	}
	return json.Marshal(alias(i))
}

func (i *ResponseOutputItem) UnmarshalJSON(data []byte) error {
	type alias ResponseOutputItem
	var raw struct {
		alias
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*i = ResponseOutputItem(raw.alias)
	if len(raw.Arguments) == 0 || string(raw.Arguments) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw.Arguments, &s); err == nil {
		i.Arguments = s
		return nil
	}
	i.ArgumentsJSON = append(i.ArgumentsJSON[:0], raw.Arguments...)
	return nil
}

// ResponseReasoningSummary is a single summary block inside a Responses
// `reasoning` output item.
type ResponseReasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponseText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

func (t ResponseText) MarshalJSON() ([]byte, error) {
	type responseText struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Annotations []any  `json:"annotations"`
	}
	annotations := t.Annotations
	if annotations == nil {
		annotations = []any{}
	}
	return json.Marshal(responseText{Type: t.Type, Text: t.Text, Annotations: annotations})
}

// ResponseInputItem is the request-side counterpart of ResponseOutputItem. It
// is decode-only: the proxy parses client input with it and never serializes it
// back onto the wire, so unlike ResponseOutputItem it deliberately keeps plain
// omitempty semantics rather than forcing per-variant required fields.
type ResponseInputItem struct {
	Type          string          `json:"type,omitempty"`
	ID            string          `json:"id,omitempty"`
	Role          string          `json:"role,omitempty"`
	Content       Content         `json:"content,omitempty"`
	CallID        string          `json:"call_id,omitempty"`
	Name          string          `json:"name,omitempty"`
	Namespace     string          `json:"namespace,omitempty"`
	Arguments     string          `json:"arguments,omitempty"`
	ArgumentsJSON json.RawMessage `json:"-"`
	Input         string          `json:"input,omitempty"`
	Output        json.RawMessage `json:"output,omitempty"`
	Status        string          `json:"status,omitempty"`
	Execution     string          `json:"execution,omitempty"`
	Tools         json.RawMessage `json:"tools,omitempty"`
}

func (i ResponseInputItem) MarshalJSON() ([]byte, error) {
	type alias ResponseInputItem
	if len(i.ArgumentsJSON) > 0 {
		return json.Marshal(struct {
			alias
			Arguments json.RawMessage `json:"arguments,omitempty"`
		}{alias: alias(i), Arguments: i.ArgumentsJSON})
	}
	return json.Marshal(alias(i))
}

func (i *ResponseInputItem) UnmarshalJSON(data []byte) error {
	type alias ResponseInputItem
	var raw struct {
		alias
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*i = ResponseInputItem(raw.alias)
	if len(raw.Arguments) == 0 || string(raw.Arguments) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw.Arguments, &s); err == nil {
		i.Arguments = s
		return nil
	}
	i.ArgumentsJSON = append(i.ArgumentsJSON[:0], raw.Arguments...)
	return nil
}

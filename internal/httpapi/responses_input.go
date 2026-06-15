package httpapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/evanlouie/copilot-api/internal/openai"
)

func parseResponsesInput(raw json.RawMessage) (openai.PromptContent, map[string]string, string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return openai.PromptContent{}, nil, "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return openai.PromptContent{Text: s}, nil, "", nil
	}
	var items []openai.ResponseInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return openai.PromptContent{}, nil, "", openai.InvalidRequest("input must be a string or an array of response input items", "input")
	}
	if responsesInputHasFunctionOutputs(items) {
		outputs := map[string]string{}
		lastOutput := -1
		for i, item := range items {
			if item.Type != "function_call_output" {
				continue
			}
			out, err := parseFunctionOutputItem(item, i)
			if err != nil {
				return openai.PromptContent{}, nil, "", err
			}
			if _, exists := outputs[item.CallID]; exists {
				return openai.PromptContent{}, nil, "", openai.InvalidRequest("duplicate function_call_output call_id", fmt.Sprintf("input.%d.call_id", i))
			}
			outputs[item.CallID] = out
			lastOutput = i
		}
		// Codex HTTP sends stateless full input arrays without previous_response_id,
		// so earlier messages/function_call/function_call_output items are history for
		// the live SDK session, not same-turn user input. Only preserve a suffix after
		// the last tool output as an explicit mixed-continuation follow-up.
		remaining := make([]openai.ResponseInputItem, 0, len(items)-lastOutput-1)
		for _, item := range items[lastOutput+1:] {
			if item.Type != "function_call_output" {
				remaining = append(remaining, item)
			}
		}
		if len(remaining) == 0 {
			return openai.PromptContent{}, outputs, "", nil
		}
		b, _ := json.Marshal(remaining)
		input, _, inputInstructions, err := parseResponsesInput(b)
		if err != nil {
			return openai.PromptContent{}, nil, "", err
		}
		return input, outputs, inputInstructions, nil
	}

	prompt, instructions, err := parseResponsesTranscriptItems(items)
	return prompt, nil, instructions, err
}

const maxResponsesFallbackTranscriptBytes = 256 * 1024

func parseResponsesFallbackInput(raw json.RawMessage) (openai.PromptContent, string, bool, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return openai.PromptContent{}, "", false, nil
	}
	var items []openai.ResponseInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return openai.PromptContent{}, "", false, openai.InvalidRequest("input must be a string or an array of response input items", "input")
	}
	if !responsesInputHasFallbackContext(items) {
		return openai.PromptContent{}, "", false, nil
	}
	prompt, instructions, err := parseResponsesTranscriptItems(items)
	if err != nil {
		return openai.PromptContent{}, "", false, err
	}
	if len(prompt.Text)+len(instructions) > maxResponsesFallbackTranscriptBytes {
		return openai.PromptContent{}, "", false, nil
	}
	return prompt, instructions, true, nil
}

func responsesInputHasFallbackContext(items []openai.ResponseInputItem) bool {
	for _, item := range items {
		if item.Type == "function_call_output" || item.Type == "reasoning" || item.Type == "item_reference" {
			continue
		}
		if item.Type != "" || item.Role != "" || item.Content.Present || item.CallID != "" || item.Name != "" || item.Arguments != "" || item.Input != "" {
			return true
		}
	}
	return false
}

func parseResponsesTranscriptItems(items []openai.ResponseInputItem) (openai.PromptContent, string, error) {
	transcriptMode := responsesInputNeedsTranscript(items)
	var prompt openai.PromptContent
	var instructions []string
	var transcript []string
	for i, item := range items {
		switch item.Type {
		case "function_call_output":
			out, err := parseFunctionOutputItem(item, i)
			if err != nil {
				return openai.PromptContent{}, "", err
			}
			transcript = append(transcript, "Function output "+item.CallID+":\n"+out)
		case "message", "":
			role := item.Role
			if role == "" {
				role = "user"
			}
			switch role {
			case "system", "developer":
				text, err := item.Content.Text()
				if err != nil {
					return openai.PromptContent{}, "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.content", i))
				}
				if strings.TrimSpace(text) != "" {
					instructions = append(instructions, responseRoleLabel(role)+":\n"+text)
				}
			case "user":
				part, err := item.Content.Prompt()
				if err != nil {
					return openai.PromptContent{}, "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.content", i))
				}
				appendPromptPart(&prompt, part, transcriptMode, &transcript, "User")
			case "assistant":
				text, err := item.Content.Text()
				if err != nil {
					return openai.PromptContent{}, "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.content", i))
				}
				if strings.TrimSpace(text) != "" {
					transcript = append(transcript, "Assistant:\n"+text)
				}
			default:
				return openai.PromptContent{}, "", openai.InvalidRequest("unsupported response input message role", fmt.Sprintf("input.%d.role", i))
			}
		case "reasoning", "item_reference":
			// Reasoning items may appear in Codex history. Item references may appear
			// in AI SDK Responses tool loops to point at prior output items; the
			// referenced function call is already represented by function_call_output.
		case "function_call":
			transcript = append(transcript, "Assistant function call "+item.Name+" "+item.CallID+":\n"+item.Arguments)
		case "custom_tool_call":
			transcript = append(transcript, "Assistant custom tool call "+item.Name+" "+item.CallID+":\n"+item.Input)
		default:
			return openai.PromptContent{}, "", openai.InvalidRequest("unsupported response input item type", fmt.Sprintf("input.%d.type", i))
		}
	}
	if len(transcript) > 0 {
		if prompt.Text != "" {
			transcript = append(transcript, "User:\n"+prompt.Text)
			prompt.Text = ""
		}
		prompt.Text = strings.Join(transcript, "\n\n")
	}
	return prompt, strings.Join(instructions, "\n\n"), nil
}
func responsesInputHasFunctionOutputs(items []openai.ResponseInputItem) bool {
	for _, item := range items {
		if item.Type == "function_call_output" {
			return true
		}
	}
	return false
}
func responsesInputNeedsTranscript(items []openai.ResponseInputItem) bool {
	for _, item := range items {
		if item.Type == "function_call" || item.Type == "custom_tool_call" || item.Type == "function_call_output" || item.Type == "item_reference" {
			return true
		}
		if (item.Type == "message" || item.Type == "") && item.Role == "assistant" {
			return true
		}
	}
	return false
}
func parseFunctionOutputItem(item openai.ResponseInputItem, i int) (string, error) {
	if item.CallID == "" {
		return "", openai.InvalidRequest("function_call_output items require call_id", fmt.Sprintf("input.%d.call_id", i))
	}
	out, err := outputRawToString(item.Output)
	if err != nil {
		return "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.output", i))
	}
	return out, nil
}
func responseRoleLabel(role string) string {
	switch role {
	case "system":
		return "System"
	case "developer":
		return "Developer"
	default:
		return role
	}
}
func appendPromptPart(prompt *openai.PromptContent, part openai.PromptContent, transcriptMode bool, transcript *[]string, label string) {
	if transcriptMode {
		if strings.TrimSpace(part.Text) != "" {
			*transcript = append(*transcript, label+":\n"+part.Text)
		}
		prompt.Images = append(prompt.Images, part.Images...)
		return
	}
	if part.Text != "" {
		if prompt.Text != "" {
			prompt.Text += "\n"
		}
		prompt.Text += part.Text
	}
	prompt.Images = append(prompt.Images, part.Images...)
}
func combineInstructions(base, extra string) string {
	if strings.TrimSpace(extra) == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return extra
	}
	return base + "\n\n" + extra
}
func outputRawToString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	if text, ok, err := outputContentPartsToString(raw); ok || err != nil {
		return text, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	switch v.(type) {
	case map[string]any, []any:
		b, _ := json.Marshal(v)
		return string(b), nil
	default:
		return "", fmt.Errorf("function_call_output output must be string, JSON object, JSON array, or content parts")
	}
}

func outputContentPartsToString(raw json.RawMessage) (string, bool, error) {
	var rawParts []json.RawMessage
	if err := json.Unmarshal(raw, &rawParts); err != nil {
		return "", false, nil
	}
	if len(rawParts) == 0 {
		return "", true, nil
	}
	parsed := make([]outputContentPart, len(rawParts))
	contentLike := make([]bool, len(rawParts))
	var parseErr error
	seenContentPart := false
	for i, rawPart := range rawParts {
		part, ok, err := parseOutputContentPart(rawPart)
		if err != nil && parseErr == nil {
			parseErr = err
		}
		parsed[i] = part
		contentLike[i] = ok
		seenContentPart = seenContentPart || ok
	}
	if !seenContentPart {
		return "", false, nil
	}
	if parseErr != nil {
		return "", true, parseErr
	}
	var b strings.Builder
	for i, part := range parsed {
		if !contentLike[i] {
			return "", true, fmt.Errorf("function_call_output content part arrays cannot mix content parts with raw JSON values")
		}
		if part.Type == "" && part.Text == "" && part.Refusal == "" {
			return "", true, fmt.Errorf("function_call_output content parts require type")
		}
		switch part.Type {
		case "text", "input_text", "output_text":
			b.WriteString(part.Text)
		case "refusal":
			b.WriteString(part.Refusal)
		case "image", "image_url", "input_image", "output_image":
			appendSeparated(&b, summarizeOutputImage(part))
		case "file", "input_file", "output_file":
			appendSeparated(&b, summarizeOutputFile(part))
		case "":
			b.WriteString(part.Text)
			b.WriteString(part.Refusal)
		default:
			return "", true, fmt.Errorf("unsupported function_call_output content part type %q", part.Type)
		}
	}
	return b.String(), true, nil
}

type outputContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Refusal  string          `json:"refusal,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
	FileID   string          `json:"file_id,omitempty"`
	FileURL  string          `json:"file_url,omitempty"`
	FileData string          `json:"file_data,omitempty"`
	Filename string          `json:"filename,omitempty"`
	Detail   string          `json:"detail,omitempty"`
}

func parseOutputContentPart(raw json.RawMessage) (outputContentPart, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return outputContentPart{}, false, err
	}
	var part outputContentPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return outputContentPart{}, outputPartFieldsLookContentLike(fields), err
	}
	contentLike := outputPartLooksContentLike(part, fields)
	if !contentLike {
		return part, false, nil
	}
	return part, true, nil
}

func outputPartLooksContentLike(part outputContentPart, fields map[string]json.RawMessage) bool {
	if isKnownOutputContentPartType(part.Type) || part.Text != "" || part.Refusal != "" || part.FileID != "" || part.FileURL != "" || part.FileData != "" || part.Filename != "" || len(part.ImageURL) > 0 {
		return true
	}
	return outputPartFieldsLookContentLike(fields)
}

func outputPartFieldsLookContentLike(fields map[string]json.RawMessage) bool {
	for _, key := range []string{"text", "refusal", "image_url", "file_id", "file_url", "file_data", "filename"} {
		if _, ok := fields[key]; ok {
			return true
		}
	}
	return false
}

func isKnownOutputContentPartType(typ string) bool {
	switch typ {
	case "text", "input_text", "output_text", "refusal", "image", "image_url", "input_image", "output_image", "file", "input_file", "output_file":
		return true
	default:
		return false
	}
}

func appendSeparated(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(s)
}

func summarizeOutputImage(part outputContentPart) string {
	ref := imageRefString(part)
	if ref == "" {
		ref = "provided"
	}
	if part.Detail != "" {
		return "[Image: " + ref + ", detail=" + sanitizeMarkerValue(part.Detail, 32) + "]"
	}
	return "[Image: " + ref + "]"
}

func imageRefString(part outputContentPart) string {
	if ref := redactedURLMarker(part.ImageURL); ref != "" {
		return ref
	}
	if part.FileID != "" {
		return "file_id present"
	}
	return ""
}

func redactedURLMarker(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return summarizeExternalRef(s)
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return summarizeExternalRef(obj.URL)
	}
	return "provided"
}

func summarizeOutputFile(part outputContentPart) string {
	refs := make([]string, 0, 5)
	if part.Filename != "" {
		refs = append(refs, "filename="+sanitizeFilename(part.Filename))
	}
	if part.FileID != "" {
		refs = append(refs, "file_id present")
	}
	if part.FileURL != "" {
		refs = append(refs, "file_url="+summarizeExternalRef(part.FileURL))
	}
	if part.FileData != "" {
		refs = append(refs, "file_data present")
	}
	if len(refs) == 0 {
		refs = append(refs, "provided")
	}
	if part.Detail != "" {
		refs = append(refs, "detail="+sanitizeMarkerValue(part.Detail, 32))
	}
	return "[File: " + strings.Join(refs, ", ") + "]"
}

func summarizeExternalRef(s string) string {
	if s == "" {
		return "provided"
	}
	if strings.HasPrefix(s, "data:") {
		mediaType := "data"
		if comma := strings.IndexByte(s, ','); comma > len("data:") {
			mediaType = sanitizeMarkerValue(s[:comma], 80)
		}
		return mediaType + ", redacted"
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "provided"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

func sanitizeFilename(s string) string {
	return sanitizeMarkerValue(filepath.Base(strings.ReplaceAll(s, "\\", "/")), 80)
}

func sanitizeMarkerValue(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		default:
			return r
		}
	}, s)
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

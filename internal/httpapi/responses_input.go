package httpapi

import (
	"encoding/json"
	"fmt"
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
		remaining := make([]openai.ResponseInputItem, 0, len(items))
		for i, item := range items {
			if item.Type != "function_call_output" {
				remaining = append(remaining, item)
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

	transcriptMode := responsesInputNeedsTranscript(items)
	var prompt openai.PromptContent
	var instructions []string
	var transcript []string
	for i, item := range items {
		switch item.Type {
		case "function_call_output":
			out, err := parseFunctionOutputItem(item, i)
			if err != nil {
				return openai.PromptContent{}, nil, "", err
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
					return openai.PromptContent{}, nil, "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.content", i))
				}
				if strings.TrimSpace(text) != "" {
					instructions = append(instructions, responseRoleLabel(role)+":\n"+text)
				}
			case "user":
				part, err := item.Content.Prompt()
				if err != nil {
					return openai.PromptContent{}, nil, "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.content", i))
				}
				appendPromptPart(&prompt, part, transcriptMode, &transcript, "User")
			case "assistant":
				text, err := item.Content.Text()
				if err != nil {
					return openai.PromptContent{}, nil, "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.content", i))
				}
				if strings.TrimSpace(text) != "" {
					transcript = append(transcript, "Assistant:\n"+text)
				}
			default:
				return openai.PromptContent{}, nil, "", openai.InvalidRequest("unsupported response input message role", fmt.Sprintf("input.%d.role", i))
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
			return openai.PromptContent{}, nil, "", openai.InvalidRequest("unsupported response input item type", fmt.Sprintf("input.%d.type", i))
		}
	}
	if len(transcript) > 0 {
		if prompt.Text != "" {
			transcript = append(transcript, "User:\n"+prompt.Text)
			prompt.Text = ""
		}
		prompt.Text = strings.Join(transcript, "\n\n")
	}
	return prompt, nil, strings.Join(instructions, "\n\n"), nil
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
		return "", fmt.Errorf("function_call_output output must be string, JSON object, JSON array, or text content parts")
	}
}

func outputContentPartsToString(raw json.RawMessage) (string, bool, error) {
	var parts []openai.ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", false, nil
	}
	if len(parts) == 0 {
		return "", true, nil
	}
	var b strings.Builder
	recognized := false
	for _, part := range parts {
		if part.FileID != "" {
			return "", true, fmt.Errorf("function_call_output file content arrays are not supported")
		}
		if len(part.ImageURL) > 0 {
			return "", true, fmt.Errorf("function_call_output image content arrays are not supported")
		}
		switch part.Type {
		case "text", "input_text", "output_text":
			recognized = true
			b.WriteString(part.Text)
		case "refusal":
			recognized = true
			b.WriteString(part.Refusal)
		case "image", "image_url", "input_image", "output_image":
			return "", true, fmt.Errorf("function_call_output image content arrays are not supported")
		case "file", "input_file", "output_file":
			return "", true, fmt.Errorf("function_call_output file content arrays are not supported")
		case "":
			if part.Text == "" && part.Refusal == "" {
				return "", false, nil
			}
			recognized = true
			b.WriteString(part.Text)
			b.WriteString(part.Refusal)
		default:
			return "", true, fmt.Errorf("unsupported function_call_output content part type %q", part.Type)
		}
	}
	if !recognized {
		return "", false, nil
	}
	return b.String(), true, nil
}

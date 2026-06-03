package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/evanlouie/copilot-api/internal/openai"
)

func parseResponsesInput(raw json.RawMessage) (openai.PromptContent, map[string]string, string, error) {
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
		for i, item := range items {
			if item.Type != "function_call_output" {
				return openai.PromptContent{}, nil, "", openai.InvalidRequest("function_call_output continuation input cannot be mixed with other input item types", fmt.Sprintf("input.%d.type", i))
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
		return openai.PromptContent{}, outputs, "", nil
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
		case "reasoning":
			// Reasoning items may appear in Codex history. They are not user-visible prompt content.
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
		if item.Type == "function_call" || item.Type == "custom_tool_call" || item.Type == "function_call_output" {
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
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
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
		return "", fmt.Errorf("function_call_output output must be string, JSON object, or JSON array")
	}
}

package openai

import "encoding/json"

// Tool wire DTOs shared by the Chat Completions and Responses requests.

type Tool struct {
	Type         string          `json:"type,omitempty"`
	Function     FunctionTool    `json:"function,omitempty"`
	Name         string          `json:"name,omitempty"`
	Description  string          `json:"description,omitempty"`
	Parameters   json.RawMessage `json:"parameters,omitempty"`
	Strict       *bool           `json:"strict,omitempty"`
	DeferLoading *bool           `json:"defer_loading,omitempty"`
	Format       json.RawMessage `json:"format,omitempty"`
	Execution    string          `json:"execution,omitempty"`
	Tools        []Tool          `json:"tools,omitempty"`
	Raw          json.RawMessage `json:"-"`
}

func (t *Tool) UnmarshalJSON(data []byte) error {
	type alias Tool
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	a.Raw = append(a.Raw[:0], data...)
	if a.Type == "function" {
		if a.Function.Name == "" {
			a.Function = FunctionTool{Name: a.Name, Description: a.Description, Parameters: a.Parameters, Strict: a.Strict}
		} else {
			if a.Name == "" {
				a.Name = a.Function.Name
			}
			if a.Description == "" {
				a.Description = a.Function.Description
			}
			if len(a.Parameters) == 0 {
				a.Parameters = a.Function.Parameters
			}
			if a.Strict == nil {
				a.Strict = a.Function.Strict
			}
		}
	}
	*t = Tool(a)
	return nil
}

type FunctionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ChatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

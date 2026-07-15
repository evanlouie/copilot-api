package sessionstore

import "encoding/json"

// The durable response schema is owned by sessionstore. Gateway mappings keep
// OpenAI wire DTO changes from silently changing persisted records.
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

func (i ResponseOutputItem) MarshalJSON() ([]byte, error) {
	type alias ResponseOutputItem
	if i.Type == "tool_search_call" && len(i.ArgumentsJSON) > 0 {
		return json.Marshal(struct {
			alias
			Arguments json.RawMessage `json:"arguments,omitempty"`
		}{alias: alias(i), Arguments: i.ArgumentsJSON})
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
	var text string
	if err := json.Unmarshal(raw.Arguments, &text); err == nil {
		i.Arguments = text
		return nil
	}
	i.ArgumentsJSON = append(i.ArgumentsJSON[:0], raw.Arguments...)
	return nil
}

type ResponseText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

func (t ResponseText) MarshalJSON() ([]byte, error) {
	type wireText ResponseText
	if t.Annotations == nil {
		t.Annotations = []any{}
	}
	return json.Marshal(wireText(t))
}

type ResponseReasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponseUsage struct {
	InputTokens         *int64                       `json:"input_tokens,omitempty"`
	InputTokensDetails  *ResponseInputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        *int64                       `json:"output_tokens,omitempty"`
	OutputTokensDetails *ResponseOutputTokensDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         *int64                       `json:"total_tokens,omitempty"`
}

func (u *ResponseUsage) UnmarshalJSON(data []byte) error {
	type current ResponseUsage
	var decoded current
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = ResponseUsage(decoded)
	var legacy struct {
		PromptTokens            *int64                       `json:"prompt_tokens"`
		PromptTokensDetails     *ResponseInputTokensDetails  `json:"prompt_tokens_details"`
		CompletionTokens        *int64                       `json:"completion_tokens"`
		CompletionTokensDetails *ResponseOutputTokensDetails `json:"completion_tokens_details"`
		TotalTokens             *int64                       `json:"total_tokens"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	if legacy.PromptTokens != nil {
		u.InputTokens = legacy.PromptTokens
	}
	if legacy.PromptTokensDetails != nil {
		u.InputTokensDetails = legacy.PromptTokensDetails
	}
	if legacy.CompletionTokens != nil {
		u.OutputTokens = legacy.CompletionTokens
	}
	if legacy.CompletionTokensDetails != nil {
		u.OutputTokensDetails = legacy.CompletionTokensDetails
	}
	if legacy.TotalTokens != nil {
		u.TotalTokens = legacy.TotalTokens
	}
	return nil
}

type ResponseInputTokensDetails struct {
	CachedTokens *int64 `json:"cached_tokens,omitempty"`
}

type ResponseOutputTokensDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

type StoredToolCatalog struct {
	SchemaVersion int              `json:"schema_version"`
	CatalogKey    string           `json:"catalog_key"`
	Tools         []StoredToolSpec `json:"tools"`
	KnownEmpty    bool             `json:"known_empty,omitempty"`
}

type StoredToolSpec struct {
	Type         string           `json:"type"`
	Name         string           `json:"name"`
	Namespace    string           `json:"namespace,omitempty"`
	Description  string           `json:"description,omitempty"`
	Parameters   json.RawMessage  `json:"parameters,omitempty"`
	Format       json.RawMessage  `json:"format,omitempty"`
	Execution    string           `json:"execution,omitempty"`
	Strict       *bool            `json:"strict,omitempty"`
	DeferLoading *bool            `json:"defer_loading,omitempty"`
	Tools        []StoredToolSpec `json:"tools,omitempty"`
}

type StoredLoadedToolEvent struct {
	SourceCallID string           `json:"source_call_id"`
	ResponseID   string           `json:"response_id"`
	Status       string           `json:"status,omitempty"`
	Execution    string           `json:"execution,omitempty"`
	RawTools     json.RawMessage  `json:"raw_tools,omitempty"`
	LoadedTools  []StoredToolSpec `json:"loaded_tools,omitempty"`
}

type StoredToolOutput struct {
	Type      string          `json:"type"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name,omitempty"`
	Output    string          `json:"output,omitempty"`
	Status    string          `json:"status,omitempty"`
	Execution string          `json:"execution,omitempty"`
	Tools     json.RawMessage `json:"tools,omitempty"`
}

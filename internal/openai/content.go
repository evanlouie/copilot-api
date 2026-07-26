package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Chat message content: the polymorphic `content` field, the prompt shape it
// decodes into, and the multimodal parts it may carry.

type Content struct {
	Raw     json.RawMessage
	IsNull  bool
	Present bool
}

func (c *Content) UnmarshalJSON(data []byte) error {
	c.Present = true
	c.Raw = append(c.Raw[:0], data...)
	c.IsNull = bytes.Equal(bytes.TrimSpace(data), []byte("null"))
	return nil
}

func (c Content) MarshalJSON() ([]byte, error) {
	if !c.Present || c.IsNull {
		return []byte("null"), nil
	}
	return c.Raw, nil
}

func NewTextContent(s string) Content {
	b, _ := json.Marshal(s)
	return Content{Raw: b, Present: true}
}

type PromptContent struct {
	Text   string
	Images []ImageInput
}

type ImageInput struct {
	URL    string
	Detail string
}

func (c Content) Text() (string, error) {
	prompt, err := c.Prompt()
	if err != nil {
		return "", err
	}
	if len(prompt.Images) > 0 {
		return "", fmt.Errorf("image content is not supported in text-only content")
	}
	return prompt.Text, nil
}

func (c Content) Prompt() (PromptContent, error) {
	if !c.Present || c.IsNull {
		return PromptContent{}, nil
	}
	var s string
	if err := json.Unmarshal(c.Raw, &s); err == nil {
		return PromptContent{Text: s}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(c.Raw, &parts); err == nil {
		var prompt PromptContent
		var b strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text", "input_text", "output_text":
				b.WriteString(p.Text)
			case "refusal":
				if p.Refusal != "" {
					b.WriteString(p.Refusal)
				}
			case "image_url", "input_image":
				image, err := p.Image()
				if err != nil {
					return PromptContent{}, err
				}
				prompt.Images = append(prompt.Images, image)
			default:
				return PromptContent{}, fmt.Errorf("unsupported content part type %q", p.Type)
			}
		}
		prompt.Text = b.String()
		return prompt, nil
	}
	return PromptContent{}, fmt.Errorf("content must be a string or content parts")
}

// ToolOutput renders tool-result content as the plain string the Copilot SDK
// consumes. Clients send tool results as a string, as content parts, or - as
// LangChain's ToolMessage and MCP bridges do - as an arbitrary JSON value; a
// non-string JSON value round-trips as its compact encoding so nothing the
// client meant is lost. This is the one definition of "valid tool output": both
// request validation and the Chat Completions handler call it.
func (c Content) ToolOutput() (string, error) {
	if !c.Present || c.IsNull {
		return "", nil
	}
	if s, err := c.Text(); err == nil {
		return s, nil
	}
	var v any
	if err := json.Unmarshal(c.Raw, &v); err != nil {
		return "", fmt.Errorf("tool output must be a string, content parts, or a JSON value")
	}
	switch v.(type) {
	case map[string]any, []any:
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("tool output must be a string, content parts, or a JSON value")
		}
		return string(b), nil
	default:
		return string(bytes.TrimSpace(c.Raw)), nil
	}
}

type ContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Refusal  string          `json:"refusal,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
	FileID   string          `json:"file_id,omitempty"`
	Detail   string          `json:"detail,omitempty"`
}

func (p ContentPart) Image() (ImageInput, error) {
	if p.FileID != "" {
		return ImageInput{}, fmt.Errorf("file_id image inputs are not supported")
	}
	if len(p.ImageURL) == 0 || string(p.ImageURL) == "null" {
		return ImageInput{}, fmt.Errorf("%s content parts require image_url", p.Type)
	}
	var url string
	if err := json.Unmarshal(p.ImageURL, &url); err == nil {
		return ImageInput{URL: url, Detail: p.Detail}, nil
	}
	var obj struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	}
	if err := json.Unmarshal(p.ImageURL, &obj); err != nil {
		return ImageInput{}, fmt.Errorf("image_url must be a string or object")
	}
	detail := p.Detail
	if detail == "" {
		detail = obj.Detail
	}
	if obj.URL == "" {
		return ImageInput{}, fmt.Errorf("image_url.url is required")
	}
	return ImageInput{URL: obj.URL, Detail: detail}, nil
}

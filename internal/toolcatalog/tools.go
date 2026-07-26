// Package toolcatalog holds the transport-free vocabulary for the Responses
// tool catalog: the normalized tool shape, the installed-catalog algebra
// (merge, identity, SDK aliasing, limits) and its persistence DTOs.
//
// Nothing here knows about HTTP or about the OpenAI wire `tools` array. The
// wire -> domain adapter lives in internal/openai (responses_tools.go), which
// is why this package must not import it: internal/toolproxy and
// internal/copilotgw share this vocabulary, and internal/openai's request
// validation calls the adapter.
package toolcatalog

import (
	"encoding/json"
	"fmt"

	"github.com/evanlouie/copilot-api/internal/apierr"
)

type ResponsesToolKind string

const (
	ToolKindFunction   ResponsesToolKind = "function"
	ToolKindCustom     ResponsesToolKind = "custom"
	ToolKindNamespace  ResponsesToolKind = "namespace"
	ToolKindToolSearch ResponsesToolKind = "tool_search"
)

// NormalizedTool is one tool as the gateway understands it, independent of the
// wire shape it arrived in.
type NormalizedTool struct {
	Kind         ResponsesToolKind
	Name         string
	Namespace    string
	Description  string
	Parameters   json.RawMessage
	Format       json.RawMessage
	Execution    string
	Strict       *bool
	DeferLoading *bool
	Children     []NormalizedTool
	Raw          json.RawMessage
}

// ResponseToolOutput is a client-supplied result for one tool call.
type ResponseToolOutput struct {
	Kind        ResponsesToolKind
	CallID      string
	Name        string
	Output      string
	Status      string
	Execution   string
	Tools       json.RawMessage
	LoadedTools []NormalizedTool
}

// ValidateCatalog rejects a tool set that cannot be installed as a whole:
// empty or duplicated namespaces, duplicate identities, or SDK name collisions.
func ValidateCatalog(tools []NormalizedTool, param string) error {
	identities := map[string]struct{}{}
	namespaces := map[string]struct{}{}
	sdkNames := map[string]string{NoToolsSentinelName: "reserved sentinel"}
	for _, tool := range tools {
		if tool.Kind == ToolKindNamespace {
			if len(tool.Children) == 0 {
				return apierr.InvalidRequest("namespace tools require at least one child tool", param)
			}
			if _, exists := namespaces[tool.Name]; exists {
				return apierr.InvalidRequest("duplicate Responses namespace tool name", param)
			}
			namespaces[tool.Name] = struct{}{}
			for _, child := range tool.Children {
				child.Namespace = tool.Name
				if err := validateFlattenedToolIdentity(child, param, identities, sdkNames); err != nil {
					return err
				}
			}
			continue
		}
		if err := validateFlattenedToolIdentity(tool, param, identities, sdkNames); err != nil {
			return err
		}
	}
	return nil
}

func validateFlattenedToolIdentity(tool NormalizedTool, param string, identities map[string]struct{}, sdkNames map[string]string) error {
	identity := NormalizedToolIdentity(tool)
	if _, exists := identities[identity]; exists {
		return apierr.InvalidRequest("duplicate Responses tool identity", param)
	}
	identities[identity] = struct{}{}
	sdkName := NormalizedToolSDKName(tool)
	if prior, exists := sdkNames[sdkName]; exists {
		return apierr.InvalidRequest(fmt.Sprintf("Responses tool SDK name collision for %q with %s", sdkName, prior), param)
	}
	sdkNames[sdkName] = identity
	return nil
}

// ValidateSchemaRaw accepts an absent or null schema and otherwise requires
// valid JSON, reporting message against param.
func ValidateSchemaRaw(raw json.RawMessage, param, message string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return ValidateJSONRaw(raw, param, message)
}

// ValidateJSONRaw requires raw to be valid JSON when present.
func ValidateJSONRaw(raw json.RawMessage, param, message string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var js any
	if err := json.Unmarshal(raw, &js); err != nil {
		return apierr.InvalidRequest(message, param)
	}
	return nil
}

// CloneRaw copies raw so a normalized tool never aliases the decoded request.
func CloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage{}, raw...)
}

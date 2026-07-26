package openai

import (
	"encoding/json"
	"fmt"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
)

// This file is the wire -> domain adapter for Responses tools: it reads the
// OpenAI `tools` array (and the `tool_search_output.tools` payload) and hands
// back the transport-free catalog vocabulary from internal/toolcatalog. The
// catalog algebra itself — merging, identity, persistence — lives there.

// NormalizeResponsesTools reads the OpenAI `tools` array into the catalog
// vocabulary. Hosted tool types this proxy cannot run are dropped (see
// IgnoredResponsesToolTypes, which lets the HTTP layer log the drop); anything
// else that fails to normalize is a 400.
func NormalizeResponsesTools(tools []Tool) ([]toolcatalog.NormalizedTool, error) {
	out := make([]toolcatalog.NormalizedTool, 0, len(tools))
	for i, tool := range tools {
		normalized, err := normalizeResponsesTool(tool, fmt.Sprintf("tools.%d", i), false)
		if err != nil {
			if canIgnoreUnsupportedResponsesTool(tool) {
				continue
			}
			return nil, err
		}
		out = append(out, normalized)
	}
	if err := toolcatalog.ValidateCatalog(out, "tools"); err != nil {
		return nil, err
	}
	return out, nil
}

// hostedResponsesToolTypes are the OpenAI-hosted or proxy-executed Responses
// tools this proxy cannot run. They are enumerated so that exactly these can be
// dropped (with a debug log) while any other unrecognized type stays a 400: a
// typo like {"type":"funcion"} must surface, not disappear.
var hostedResponsesToolTypes = map[string]struct{}{
	"web_search":           {},
	"web_search_preview":   {},
	"image_generation":     {},
	"mcp":                  {},
	"file_search":          {},
	"computer_use_preview": {},
	"code_interpreter":     {},
}

func canIgnoreUnsupportedResponsesTool(tool Tool) bool {
	_, hosted := hostedResponsesToolTypes[tool.Type]
	return hosted
}

// IgnoredResponsesToolTypes lists the hosted tool types that
// NormalizeResponsesTools drops, so the HTTP layer can report them at debug
// level.
func IgnoredResponsesToolTypes(tools []Tool) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, tool := range tools {
		if !canIgnoreUnsupportedResponsesTool(tool) {
			continue
		}
		if _, exists := seen[tool.Type]; exists {
			continue
		}
		seen[tool.Type] = struct{}{}
		out = append(out, tool.Type)
	}
	return out
}

func NormalizeToolSearchOutputTools(raw json.RawMessage, param string) ([]toolcatalog.NormalizedTool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if len(raw) > toolcatalog.MaxLoadedRawToolsBytes {
		return nil, apierr.InvalidRequest("tool_search_output.tools is too large", param)
	}
	var tools []Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, apierr.InvalidRequest("tool_search_output.tools must be an array of tool specs", param)
	}
	out := make([]toolcatalog.NormalizedTool, 0, len(tools))
	for i, tool := range tools {
		normalized, err := normalizeLoadableToolSearchTool(tool, fmt.Sprintf("%s.%d", param, i))
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	if err := toolcatalog.ValidateCatalog(out, param); err != nil {
		return nil, err
	}
	if toolcatalog.FlattenedToolCount(out) > toolcatalog.MaxLoadedToolCount {
		return nil, apierr.InvalidRequest("tool_search_output.tools contains too many loadable tools", param)
	}
	if err := toolcatalog.ValidateLoadedToolLimits(out); err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeResponsesTool(tool Tool, param string, namespaceChild bool) (toolcatalog.NormalizedTool, error) {
	typ := tool.Type
	if namespaceChild && typ == "" {
		typ = "function"
	}
	switch typ {
	case "function":
		name := tool.Function.Name
		if name == "" {
			name = tool.Name
		}
		if name == "" {
			return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("function tools require name", toolNameParam(param, namespaceChild))
		}
		description := tool.Function.Description
		if description == "" {
			description = tool.Description
		}
		parameters := tool.Function.Parameters
		if len(parameters) == 0 {
			parameters = tool.Parameters
		}
		strict := tool.Function.Strict
		if strict == nil {
			strict = tool.Strict
		}
		if err := toolcatalog.ValidateSchemaRaw(parameters, toolParametersParam(param, namespaceChild), "function parameters must be valid JSON Schema"); err != nil {
			return toolcatalog.NormalizedTool{}, err
		}
		return toolcatalog.NormalizedTool{Kind: toolcatalog.ToolKindFunction, Name: name, Description: description, Parameters: toolcatalog.CloneRaw(parameters), Strict: strict, DeferLoading: tool.DeferLoading, Raw: toolcatalog.CloneRaw(tool.Raw)}, nil
	case "custom":
		if namespaceChild {
			return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("namespace tools may only contain function tools", param+".type")
		}
		if tool.Name == "" {
			return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("custom tools require name", param+".name")
		}
		if err := toolcatalog.ValidateJSONRaw(tool.Format, param+".format", "custom tool format must be valid JSON"); err != nil {
			return toolcatalog.NormalizedTool{}, err
		}
		return toolcatalog.NormalizedTool{Kind: toolcatalog.ToolKindCustom, Name: tool.Name, Description: tool.Description, Format: toolcatalog.CloneRaw(tool.Format), Strict: tool.Strict, DeferLoading: tool.DeferLoading, Raw: toolcatalog.CloneRaw(tool.Raw)}, nil
	case "namespace":
		if namespaceChild {
			return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("nested namespace tools are not supported", param+".type")
		}
		if tool.Name == "" {
			return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("namespace tools require name", param+".name")
		}
		if len(tool.Tools) == 0 {
			return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("namespace tools require at least one child tool", param+".tools")
		}
		children := make([]toolcatalog.NormalizedTool, 0, len(tool.Tools))
		seen := map[string]struct{}{}
		for i, child := range tool.Tools {
			normalized, err := normalizeResponsesTool(child, fmt.Sprintf("%s.tools.%d", param, i), true)
			if err != nil {
				return toolcatalog.NormalizedTool{}, err
			}
			if _, exists := seen[normalized.Name]; exists {
				return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("duplicate namespace child tool name", fmt.Sprintf("%s.tools.%d.name", param, i))
			}
			seen[normalized.Name] = struct{}{}
			normalized.Namespace = tool.Name
			children = append(children, normalized)
		}
		return toolcatalog.NormalizedTool{Kind: toolcatalog.ToolKindNamespace, Name: tool.Name, Description: tool.Description, DeferLoading: tool.DeferLoading, Children: children, Raw: toolcatalog.CloneRaw(tool.Raw)}, nil
	case "tool_search":
		if namespaceChild {
			return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("namespace tools may only contain function tools", param+".type")
		}
		execution := tool.Execution
		if execution == "" {
			execution = "client"
		}
		if execution != "client" {
			return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("tool_search execution must be client", param+".execution")
		}
		if err := toolcatalog.ValidateSchemaRaw(tool.Parameters, param+".parameters", "tool_search parameters must be valid JSON Schema"); err != nil {
			return toolcatalog.NormalizedTool{}, err
		}
		return toolcatalog.NormalizedTool{Kind: toolcatalog.ToolKindToolSearch, Name: "tool_search", Description: tool.Description, Parameters: toolcatalog.CloneRaw(tool.Parameters), Execution: execution, Raw: toolcatalog.CloneRaw(tool.Raw)}, nil
	case "":
		return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("tool type is required", param+".type")
	default:
		if _, hosted := hostedResponsesToolTypes[typ]; hosted {
			return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("hosted or proxy-executed Responses tools are not supported", param+".type")
		}
		return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("unsupported Responses tool type", param+".type")
	}
}

func normalizeLoadableToolSearchTool(tool Tool, param string) (toolcatalog.NormalizedTool, error) {
	typ := tool.Type
	if typ == "" {
		typ = "function"
	}
	switch typ {
	case "function":
		if err := validateLoadableFunctionToolFields(tool.Raw, param); err != nil {
			return toolcatalog.NormalizedTool{}, err
		}
		return normalizeResponsesTool(tool, param, false)
	case "namespace":
		if err := validateLoadableNamespaceToolFields(tool, param); err != nil {
			return toolcatalog.NormalizedTool{}, err
		}
		return normalizeResponsesTool(tool, param, false)
	case "custom", "tool_search":
		return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("tool_search_output.tools may only contain loadable function or namespace tools", param+".type")
	default:
		if _, hosted := hostedResponsesToolTypes[typ]; hosted {
			return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("hosted or proxy-executed Responses tools are not supported", param+".type")
		}
		return toolcatalog.NormalizedTool{}, apierr.InvalidRequest("unsupported tool_search_output tool type", param+".type")
	}
}

func validateLoadableFunctionToolFields(raw json.RawMessage, param string) error {
	if len(raw) == 0 {
		return nil
	}
	fields, err := rawObjectFields(raw)
	if err != nil {
		return apierr.InvalidRequest("tool_search_output.tools entries must be JSON objects", param)
	}
	allowed := map[string]struct{}{"type": {}, "function": {}, "name": {}, "description": {}, "parameters": {}, "strict": {}, "defer_loading": {}}
	if err := rejectUnknownFields(fields, allowed, param); err != nil {
		return err
	}
	if nested, ok := fields["function"]; ok && len(nested) > 0 && string(nested) != "null" {
		for _, duplicate := range []string{"name", "description", "parameters", "strict"} {
			if _, exists := fields[duplicate]; exists {
				return apierr.InvalidRequest("function tools in tool_search_output.tools cannot mix top-level and nested function fields", param+"."+duplicate)
			}
		}
		nestedFields, err := rawObjectFields(nested)
		if err != nil {
			return apierr.InvalidRequest("function tool function field must be an object", param+".function")
		}
		if err := rejectUnknownFields(nestedFields, map[string]struct{}{"name": {}, "description": {}, "parameters": {}, "strict": {}}, param+".function"); err != nil {
			return err
		}
	}
	return nil
}

func validateLoadableNamespaceToolFields(tool Tool, param string) error {
	if len(tool.Raw) > 0 {
		fields, err := rawObjectFields(tool.Raw)
		if err != nil {
			return apierr.InvalidRequest("tool_search_output.tools entries must be JSON objects", param)
		}
		allowed := map[string]struct{}{"type": {}, "name": {}, "description": {}, "defer_loading": {}, "tools": {}}
		if err := rejectUnknownFields(fields, allowed, param); err != nil {
			return err
		}
	}
	for i, child := range tool.Tools {
		if err := validateLoadableFunctionToolFields(child.Raw, fmt.Sprintf("%s.tools.%d", param, i)); err != nil {
			return err
		}
	}
	return nil
}

func rawObjectFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("not an object")
	}
	return fields, nil
}

func rejectUnknownFields(fields map[string]json.RawMessage, allowed map[string]struct{}, param string) error {
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			return apierr.InvalidRequest("unsupported field in tool_search_output.tools loadable tool", param+"."+name)
		}
	}
	return nil
}

func toolNameParam(param string, namespaceChild bool) string {
	if namespaceChild {
		return param + ".name"
	}
	return param + ".function.name"
}

func toolParametersParam(param string, namespaceChild bool) string {
	if namespaceChild {
		return param + ".parameters"
	}
	return param + ".function.parameters"
}

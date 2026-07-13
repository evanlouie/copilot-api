package openai

import (
	"encoding/json"
	"fmt"
)

type ResponsesToolKind string

const (
	ToolKindFunction   ResponsesToolKind = "function"
	ToolKindCustom     ResponsesToolKind = "custom"
	ToolKindNamespace  ResponsesToolKind = "namespace"
	ToolKindToolSearch ResponsesToolKind = "tool_search"
)

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

func NormalizeResponsesTools(tools []Tool) ([]NormalizedTool, error) {
	return NormalizeResponsesToolsWithMode(tools, true)
}

func NormalizeResponsesToolsWithMode(tools []Tool, strict bool) ([]NormalizedTool, error) {
	out := make([]NormalizedTool, 0, len(tools))
	for i, tool := range tools {
		normalized, err := normalizeResponsesTool(tool, fmt.Sprintf("tools.%d", i), false)
		if err != nil {
			if !strict && canIgnoreUnsupportedResponsesTool(tool) {
				continue
			}
			return nil, err
		}
		out = append(out, normalized)
	}
	if err := validateNormalizedToolCatalog(out, "tools"); err != nil {
		return nil, err
	}
	return out, nil
}

func canIgnoreUnsupportedResponsesTool(tool Tool) bool {
	switch tool.Type {
	case "", "function", "custom", "namespace", "tool_search":
		return false
	default:
		return true
	}
}

func ValidateToolSearchOutputTools(raw json.RawMessage, param string) error {
	_, err := NormalizeToolSearchOutputTools(raw, param)
	return err
}

func NormalizeToolSearchOutputTools(raw json.RawMessage, param string) ([]NormalizedTool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if len(raw) > MaxLoadedRawToolsBytes {
		return nil, InvalidRequest("tool_search_output.tools is too large", param)
	}
	var tools []Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, InvalidRequest("tool_search_output.tools must be an array of tool specs", param)
	}
	out := make([]NormalizedTool, 0, len(tools))
	for i, tool := range tools {
		normalized, err := normalizeLoadableToolSearchTool(tool, fmt.Sprintf("%s.%d", param, i))
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	if err := validateNormalizedToolCatalog(out, param); err != nil {
		return nil, err
	}
	if flattenedToolCount(out) > MaxLoadedToolCount {
		return nil, InvalidRequest("tool_search_output.tools contains too many loadable tools", param)
	}
	if err := validateLoadedToolLimits(out); err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeResponsesTool(tool Tool, param string, namespaceChild bool) (NormalizedTool, error) {
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
			return NormalizedTool{}, InvalidRequest("function tools require name", toolNameParam(param, namespaceChild))
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
		if err := validateSchemaRaw(parameters, toolParametersParam(param, namespaceChild), "function parameters must be valid JSON Schema"); err != nil {
			return NormalizedTool{}, err
		}
		return NormalizedTool{Kind: ToolKindFunction, Name: name, Description: description, Parameters: cloneRaw(parameters), Strict: strict, DeferLoading: tool.DeferLoading, Raw: cloneRaw(tool.Raw)}, nil
	case "custom":
		if namespaceChild {
			return NormalizedTool{}, InvalidRequest("namespace tools may only contain function tools", param+".type")
		}
		if tool.Name == "" {
			return NormalizedTool{}, InvalidRequest("custom tools require name", param+".name")
		}
		if err := validateJSONRaw(tool.Format, param+".format", "custom tool format must be valid JSON"); err != nil {
			return NormalizedTool{}, err
		}
		return NormalizedTool{Kind: ToolKindCustom, Name: tool.Name, Description: tool.Description, Format: cloneRaw(tool.Format), Strict: tool.Strict, DeferLoading: tool.DeferLoading, Raw: cloneRaw(tool.Raw)}, nil
	case "namespace":
		if namespaceChild {
			return NormalizedTool{}, InvalidRequest("nested namespace tools are not supported", param+".type")
		}
		if tool.Name == "" {
			return NormalizedTool{}, InvalidRequest("namespace tools require name", param+".name")
		}
		if len(tool.Tools) == 0 {
			return NormalizedTool{}, InvalidRequest("namespace tools require at least one child tool", param+".tools")
		}
		children := make([]NormalizedTool, 0, len(tool.Tools))
		seen := map[string]struct{}{}
		for i, child := range tool.Tools {
			normalized, err := normalizeResponsesTool(child, fmt.Sprintf("%s.tools.%d", param, i), true)
			if err != nil {
				return NormalizedTool{}, err
			}
			if _, exists := seen[normalized.Name]; exists {
				return NormalizedTool{}, InvalidRequest("duplicate namespace child tool name", fmt.Sprintf("%s.tools.%d.name", param, i))
			}
			seen[normalized.Name] = struct{}{}
			normalized.Namespace = tool.Name
			children = append(children, normalized)
		}
		return NormalizedTool{Kind: ToolKindNamespace, Name: tool.Name, Description: tool.Description, DeferLoading: tool.DeferLoading, Children: children, Raw: cloneRaw(tool.Raw)}, nil
	case "tool_search":
		if namespaceChild {
			return NormalizedTool{}, InvalidRequest("namespace tools may only contain function tools", param+".type")
		}
		execution := tool.Execution
		if execution == "" {
			execution = "client"
		}
		if execution != "client" {
			return NormalizedTool{}, InvalidRequest("tool_search execution must be client", param+".execution")
		}
		if err := validateSchemaRaw(tool.Parameters, param+".parameters", "tool_search parameters must be valid JSON Schema"); err != nil {
			return NormalizedTool{}, err
		}
		return NormalizedTool{Kind: ToolKindToolSearch, Name: "tool_search", Description: tool.Description, Parameters: cloneRaw(tool.Parameters), Execution: execution, Raw: cloneRaw(tool.Raw)}, nil
	case "web_search", "web_search_preview", "image_generation", "mcp", "file_search", "computer_use_preview", "code_interpreter":
		return NormalizedTool{}, InvalidRequest("hosted or proxy-executed Responses tools are not supported", param+".type")
	case "":
		return NormalizedTool{}, InvalidRequest("tool type is required", param+".type")
	default:
		return NormalizedTool{}, InvalidRequest("unsupported Responses tool type", param+".type")
	}
}

func normalizeLoadableToolSearchTool(tool Tool, param string) (NormalizedTool, error) {
	typ := tool.Type
	if typ == "" {
		typ = "function"
	}
	switch typ {
	case "function":
		if err := validateLoadableFunctionToolFields(tool.Raw, param); err != nil {
			return NormalizedTool{}, err
		}
		return normalizeResponsesTool(tool, param, false)
	case "namespace":
		if err := validateLoadableNamespaceToolFields(tool, param); err != nil {
			return NormalizedTool{}, err
		}
		return normalizeResponsesTool(tool, param, false)
	case "custom", "tool_search":
		return NormalizedTool{}, InvalidRequest("tool_search_output.tools may only contain loadable function or namespace tools", param+".type")
	case "web_search", "web_search_preview", "image_generation", "mcp", "file_search", "computer_use_preview", "code_interpreter":
		return NormalizedTool{}, InvalidRequest("hosted or proxy-executed Responses tools are not supported", param+".type")
	default:
		return NormalizedTool{}, InvalidRequest("unsupported tool_search_output tool type", param+".type")
	}
}

func validateNormalizedToolCatalog(tools []NormalizedTool, param string) error {
	identities := map[string]struct{}{}
	namespaces := map[string]struct{}{}
	sdkNames := map[string]string{NoToolsSentinelName: "reserved sentinel"}
	for _, tool := range tools {
		if tool.Kind == ToolKindNamespace {
			if len(tool.Children) == 0 {
				return InvalidRequest("namespace tools require at least one child tool", param)
			}
			if _, exists := namespaces[tool.Name]; exists {
				return InvalidRequest("duplicate Responses namespace tool name", param)
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
		return InvalidRequest("duplicate Responses tool identity", param)
	}
	identities[identity] = struct{}{}
	sdkName := NormalizedToolSDKName(tool)
	if prior, exists := sdkNames[sdkName]; exists {
		return InvalidRequest(fmt.Sprintf("Responses tool SDK name collision for %q with %s", sdkName, prior), param)
	}
	sdkNames[sdkName] = identity
	return nil
}

func validateLoadableFunctionToolFields(raw json.RawMessage, param string) error {
	if len(raw) == 0 {
		return nil
	}
	fields, err := rawObjectFields(raw)
	if err != nil {
		return InvalidRequest("tool_search_output.tools entries must be JSON objects", param)
	}
	allowed := map[string]struct{}{"type": {}, "function": {}, "name": {}, "description": {}, "parameters": {}, "strict": {}, "defer_loading": {}}
	if err := rejectUnknownFields(fields, allowed, param); err != nil {
		return err
	}
	if nested, ok := fields["function"]; ok && len(nested) > 0 && string(nested) != "null" {
		for _, duplicate := range []string{"name", "description", "parameters", "strict"} {
			if _, exists := fields[duplicate]; exists {
				return InvalidRequest("function tools in tool_search_output.tools cannot mix top-level and nested function fields", param+"."+duplicate)
			}
		}
		nestedFields, err := rawObjectFields(nested)
		if err != nil {
			return InvalidRequest("function tool function field must be an object", param+".function")
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
			return InvalidRequest("tool_search_output.tools entries must be JSON objects", param)
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
			return InvalidRequest("unsupported field in tool_search_output.tools loadable tool", param+"."+name)
		}
	}
	return nil
}

func validateSchemaRaw(raw json.RawMessage, param, message string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return validateJSONRaw(raw, param, message)
}

func validateJSONRaw(raw json.RawMessage, param, message string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var js any
	if err := json.Unmarshal(raw, &js); err != nil {
		return InvalidRequest(message, param)
	}
	return nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage{}, raw...)
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

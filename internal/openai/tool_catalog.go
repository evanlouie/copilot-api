package openai

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	ToolCatalogSchemaVersion = 1
	NoToolsSentinelName      = "__copilot_api_no_tools__"

	MaxLoadedToolCount        = 128
	MaxInstalledToolCount     = 512
	MaxLoadedToolSchemaBytes  = 64 * 1024
	MaxLoadedCatalogBytes     = 512 * 1024
	MaxLoadedRawToolsBytes    = 512 * 1024
	MaxLoadedDescriptionBytes = 8 * 1024
)

var ResponsesSDKToolNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

type ToolCatalog struct {
	Tools []NormalizedTool
}

type LoadedToolEvent struct {
	SourceCallID string
	ResponseID   string
	Status       string
	Execution    string
	RawTools     json.RawMessage
	LoadedTools  []NormalizedTool
}

type StoredToolCatalog struct {
	SchemaVersion int              `json:"schema_version"`
	CatalogKey    string           `json:"catalog_key"`
	Tools         []StoredToolSpec `json:"tools"`
	KnownEmpty    bool             `json:"known_empty,omitempty"`
}

type StoredToolSpec struct {
	Type         ResponsesToolKind `json:"type"`
	Name         string            `json:"name"`
	Namespace    string            `json:"namespace,omitempty"`
	Description  string            `json:"description,omitempty"`
	Parameters   json.RawMessage   `json:"parameters,omitempty"`
	Format       json.RawMessage   `json:"format,omitempty"`
	Execution    string            `json:"execution,omitempty"`
	Strict       *bool             `json:"strict,omitempty"`
	DeferLoading *bool             `json:"defer_loading,omitempty"`
	Tools        []StoredToolSpec  `json:"tools,omitempty"`
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

func NewToolCatalog(tools []NormalizedTool) (ToolCatalog, error) {
	cloned := cloneNormalizedTools(tools, false)
	canonicalizeNamespaceChildren(cloned)
	if err := validateNormalizedToolCatalog(cloned, "tools"); err != nil {
		return ToolCatalog{}, err
	}
	if err := validateInstalledToolCount(cloned, "tools"); err != nil {
		return ToolCatalog{}, err
	}
	return ToolCatalog{Tools: cloned}, nil
}

func ToolCatalogFromStored(stored *StoredToolCatalog) (ToolCatalog, bool, error) {
	if stored == nil {
		return ToolCatalog{}, false, nil
	}
	tools := make([]NormalizedTool, 0, len(stored.Tools))
	for i, spec := range stored.Tools {
		tool, err := normalizedToolFromStored(spec, fmt.Sprintf("installed_tool_catalog.tools.%d", i))
		if err != nil {
			return ToolCatalog{}, true, err
		}
		tools = append(tools, tool)
	}
	catalog, err := NewToolCatalog(tools)
	if err != nil {
		return ToolCatalog{}, true, err
	}
	if stored.CatalogKey != "" && catalog.Key() != stored.CatalogKey {
		return ToolCatalog{}, true, InvalidRequest("stored tool catalog key mismatch", "previous_response_id")
	}
	return catalog, true, nil
}

func (c ToolCatalog) WithoutRaw() ToolCatalog {
	return ToolCatalog{Tools: cloneNormalizedTools(c.Tools, false)}
}

func (c ToolCatalog) Flatten() []NormalizedTool {
	return cloneNormalizedTools(c.Tools, true)
}

func (c ToolCatalog) StoredDTO() *StoredToolCatalog {
	tools := c.WithoutRaw().Tools
	stored := &StoredToolCatalog{SchemaVersion: ToolCatalogSchemaVersion, CatalogKey: c.Key(), KnownEmpty: len(tools) == 0}
	stored.Tools = make([]StoredToolSpec, 0, len(tools))
	for _, tool := range tools {
		stored.Tools = append(stored.Tools, storedToolSpecFromNormalized(tool))
	}
	return stored
}

func (c ToolCatalog) Key() string {
	b, err := json.Marshal(comparableTools(c.Tools))
	if err != nil {
		return ""
	}
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}

func (c ToolCatalog) MergeLoaded(sourceCallID string, loaded []NormalizedTool) (ToolCatalog, error) {
	if len(loaded) == 0 {
		return c.WithoutRaw(), nil
	}
	if flattenedToolCount(loaded) > MaxLoadedToolCount {
		return ToolCatalog{}, InvalidRequest("tool_search_output.tools contains too many loadable tools", "input")
	}
	if err := validateLoadedToolLimits(loaded); err != nil {
		return ToolCatalog{}, err
	}
	return c.mergeTools(loaded, true, "tool_search_output.tools")
}

func (c ToolCatalog) MergeRequestTools(tools []NormalizedTool) (ToolCatalog, error) {
	if len(tools) == 0 {
		return c.WithoutRaw(), nil
	}
	return c.mergeTools(tools, false, "tools")
}

func (c ToolCatalog) mergeTools(tools []NormalizedTool, loadedOnly bool, param string) (ToolCatalog, error) {
	merged := c.WithoutRaw()
	incoming := cloneNormalizedTools(tools, false)
	canonicalizeNamespaceChildren(incoming)
	if err := validateNormalizedToolCatalog(incoming, param); err != nil {
		return ToolCatalog{}, err
	}
	for _, tool := range incoming {
		var err error
		merged.Tools, err = mergeCatalogTool(merged.Tools, tool, loadedOnly)
		if err != nil {
			return ToolCatalog{}, err
		}
	}
	if err := validateNormalizedToolCatalog(merged.Tools, "tools"); err != nil {
		return ToolCatalog{}, err
	}
	if err := validateInstalledToolCount(merged.Tools, "tools"); err != nil {
		return ToolCatalog{}, err
	}
	if err := validateStoredCatalogSize(merged); err != nil {
		return ToolCatalog{}, err
	}
	return merged, nil
}

func StoredLoadedToolEventFromLoaded(e LoadedToolEvent) StoredLoadedToolEvent {
	loaded := cloneNormalizedTools(e.LoadedTools, false)
	out := StoredLoadedToolEvent{SourceCallID: e.SourceCallID, ResponseID: e.ResponseID, Status: e.Status, Execution: e.Execution, RawTools: cloneRaw(e.RawTools)}
	out.LoadedTools = make([]StoredToolSpec, 0, len(loaded))
	for _, tool := range loaded {
		out.LoadedTools = append(out.LoadedTools, storedToolSpecFromNormalized(tool))
	}
	return out
}

func StoredToolOutputFromResponse(out ResponseToolOutput) StoredToolOutput {
	return StoredToolOutput{Type: responseToolOutputType(out.Kind), CallID: out.CallID, Name: out.Name, Output: out.Output, Status: out.Status, Execution: out.Execution, Tools: cloneRaw(out.Tools)}
}

func StoredToolOutputsFromMap(outputs map[string]ResponseToolOutput) []StoredToolOutput {
	ids := make([]string, 0, len(outputs))
	for id := range outputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	stored := make([]StoredToolOutput, 0, len(ids))
	for _, id := range ids {
		stored = append(stored, StoredToolOutputFromResponse(outputs[id]))
	}
	return stored
}

func responseToolOutputType(kind ResponsesToolKind) string {
	switch kind {
	case ToolKindCustom:
		return "custom_tool_call_output"
	case ToolKindToolSearch:
		return "tool_search_output"
	default:
		return "function_call_output"
	}
}

func mergeCatalogTool(existing []NormalizedTool, incoming NormalizedTool, loadedOnly bool) ([]NormalizedTool, error) {
	if loadedOnly && incoming.Kind != ToolKindFunction && incoming.Kind != ToolKindNamespace {
		return nil, InvalidRequest("tool_search_output.tools may only install function or namespace tools", "input")
	}
	if incoming.Kind == ToolKindNamespace {
		return mergeLoadedNamespace(existing, incoming)
	}
	identity := NormalizedToolIdentity(incoming)
	for _, tool := range existing {
		if tool.Kind == ToolKindNamespace {
			for _, child := range tool.Children {
				child.Namespace = tool.Name
				if NormalizedToolIdentity(child) == identity {
					if normalizedToolSemanticKey(child) == normalizedToolSemanticKey(incoming) {
						return existing, nil
					}
					return nil, InvalidRequest("Responses tool conflicts with an installed tool", "input")
				}
			}
			continue
		}
		if NormalizedToolIdentity(tool) == identity {
			if normalizedToolSemanticKey(tool) == normalizedToolSemanticKey(incoming) {
				return existing, nil
			}
			return nil, InvalidRequest("Responses tool conflicts with an installed tool", "input")
		}
	}
	return append(existing, incoming), nil
}

func mergeLoadedNamespace(existing []NormalizedTool, loaded NormalizedTool) ([]NormalizedTool, error) {
	loaded.Children = cloneNormalizedTools(loaded.Children, false)
	for i := range loaded.Children {
		loaded.Children[i].Namespace = loaded.Name
	}
	for i := range existing {
		if existing[i].Kind != ToolKindNamespace || existing[i].Name != loaded.Name {
			continue
		}
		if namespaceHeaderKey(existing[i]) != namespaceHeaderKey(loaded) {
			return nil, InvalidRequest("tool_search_output.tools conflicts with an installed namespace", "input")
		}
		children := append([]NormalizedTool{}, existing[i].Children...)
		for _, child := range loaded.Children {
			found := -1
			for j, existingChild := range children {
				existingChild.Namespace = loaded.Name
				if NormalizedToolIdentity(existingChild) == NormalizedToolIdentity(child) {
					found = j
					break
				}
			}
			if found >= 0 {
				existingChild := children[found]
				existingChild.Namespace = loaded.Name
				if normalizedToolSemanticKey(existingChild) != normalizedToolSemanticKey(child) {
					return nil, InvalidRequest("tool_search_output.tools conflicts with an installed namespace child", "input")
				}
				continue
			}
			children = append(children, child)
		}
		existing[i].Children = children
		return existing, nil
	}
	return append(existing, loaded), nil
}

func NormalizedToolIdentity(tool NormalizedTool) string {
	if tool.Namespace != "" {
		return string(tool.Kind) + ":" + tool.Namespace + "." + tool.Name
	}
	return string(tool.Kind) + ":" + tool.Name
}

func NormalizedToolSDKName(tool NormalizedTool) string {
	public := tool.Name
	if tool.Namespace != "" {
		public = tool.Namespace + "__" + tool.Name
	}
	if tool.Kind == ToolKindToolSearch {
		public = "tool_search"
	}
	if ResponsesSDKToolNameRE.MatchString(public) && public != NoToolsSentinelName {
		return public
	}
	return SafeResponsesSDKAlias(public)
}

func SafeResponsesSDKAlias(public string) string {
	var b strings.Builder
	for _, r := range public {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	alias := b.String()
	if alias == "" || !((alias[0] >= 'a' && alias[0] <= 'z') || (alias[0] >= 'A' && alias[0] <= 'Z') || alias[0] == '_') {
		alias = "tool_" + alias
	}
	h := sha1.Sum([]byte(public))
	suffix := "_" + hex.EncodeToString(h[:])[:10]
	if len(alias)+len(suffix) > 64 {
		alias = strings.TrimRight(alias[:64-len(suffix)], "_-")
		if alias == "" {
			alias = "tool"
		}
	}
	return alias + suffix
}

func normalizedToolFromStored(spec StoredToolSpec, param string) (NormalizedTool, error) {
	tool := NormalizedTool{Kind: spec.Type, Name: spec.Name, Namespace: spec.Namespace, Description: spec.Description, Parameters: cloneRaw(spec.Parameters), Format: cloneRaw(spec.Format), Execution: spec.Execution, Strict: spec.Strict, DeferLoading: spec.DeferLoading}
	switch spec.Type {
	case ToolKindFunction:
		if tool.Name == "" {
			return NormalizedTool{}, InvalidRequest("stored function tool requires name", param+".name")
		}
		if err := validateSchemaRaw(tool.Parameters, param+".parameters", "function parameters must be valid JSON Schema"); err != nil {
			return NormalizedTool{}, err
		}
	case ToolKindCustom:
		if tool.Name == "" {
			return NormalizedTool{}, InvalidRequest("stored custom tool requires name", param+".name")
		}
		if err := validateJSONRaw(tool.Format, param+".format", "custom tool format must be valid JSON"); err != nil {
			return NormalizedTool{}, err
		}
	case ToolKindToolSearch:
		tool.Name = "tool_search"
		if tool.Execution == "" {
			tool.Execution = "client"
		}
		if tool.Execution != "client" {
			return NormalizedTool{}, InvalidRequest("stored tool_search execution must be client", param+".execution")
		}
		if err := validateSchemaRaw(tool.Parameters, param+".parameters", "tool_search parameters must be valid JSON Schema"); err != nil {
			return NormalizedTool{}, err
		}
	case ToolKindNamespace:
		if tool.Name == "" {
			return NormalizedTool{}, InvalidRequest("stored namespace tool requires name", param+".name")
		}
		tool.Children = make([]NormalizedTool, 0, len(spec.Tools))
		for i, childSpec := range spec.Tools {
			child, err := normalizedToolFromStored(childSpec, fmt.Sprintf("%s.tools.%d", param, i))
			if err != nil {
				return NormalizedTool{}, err
			}
			if child.Kind != ToolKindFunction {
				return NormalizedTool{}, InvalidRequest("stored namespace tools may only contain function tools", fmt.Sprintf("%s.tools.%d.type", param, i))
			}
			child.Namespace = tool.Name
			tool.Children = append(tool.Children, child)
		}
	default:
		return NormalizedTool{}, InvalidRequest("stored tool catalog contains unsupported tool type", param+".type")
	}
	return tool, nil
}

func storedToolSpecFromNormalized(tool NormalizedTool) StoredToolSpec {
	spec := StoredToolSpec{Type: tool.Kind, Name: tool.Name, Namespace: tool.Namespace, Description: tool.Description, Parameters: cloneRaw(tool.Parameters), Format: cloneRaw(tool.Format), Execution: tool.Execution, Strict: tool.Strict, DeferLoading: tool.DeferLoading}
	if tool.Kind == ToolKindNamespace {
		spec.Tools = make([]StoredToolSpec, 0, len(tool.Children))
		for _, child := range tool.Children {
			child.Namespace = ""
			spec.Tools = append(spec.Tools, storedToolSpecFromNormalized(child))
		}
	}
	return spec
}

func canonicalizeNamespaceChildren(tools []NormalizedTool) {
	for i := range tools {
		if tools[i].Kind != ToolKindNamespace {
			continue
		}
		for j := range tools[i].Children {
			tools[i].Children[j].Namespace = tools[i].Name
		}
	}
}

func cloneNormalizedTools(tools []NormalizedTool, keepRaw bool) []NormalizedTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]NormalizedTool, len(tools))
	for i, tool := range tools {
		out[i] = cloneNormalizedTool(tool, keepRaw)
	}
	return out
}

func cloneNormalizedTool(tool NormalizedTool, keepRaw bool) NormalizedTool {
	out := tool
	out.Parameters = cloneRaw(tool.Parameters)
	out.Format = cloneRaw(tool.Format)
	if keepRaw {
		out.Raw = cloneRaw(tool.Raw)
	} else {
		out.Raw = nil
	}
	out.Children = cloneNormalizedTools(tool.Children, keepRaw)
	return out
}

type comparableNormalizedTool struct {
	Kind         ResponsesToolKind          `json:"kind"`
	Name         string                     `json:"name"`
	Namespace    string                     `json:"namespace,omitempty"`
	Description  string                     `json:"description,omitempty"`
	Parameters   string                     `json:"parameters,omitempty"`
	Format       string                     `json:"format,omitempty"`
	Execution    string                     `json:"execution,omitempty"`
	Strict       *bool                      `json:"strict,omitempty"`
	DeferLoading *bool                      `json:"defer_loading,omitempty"`
	Children     []comparableNormalizedTool `json:"children,omitempty"`
}

func comparableTools(tools []NormalizedTool) []comparableNormalizedTool {
	out := make([]comparableNormalizedTool, 0, len(tools))
	for _, tool := range tools {
		children := comparableTools(tool.Children)
		out = append(out, comparableNormalizedTool{
			Kind:         tool.Kind,
			Name:         tool.Name,
			Namespace:    tool.Namespace,
			Description:  tool.Description,
			Parameters:   CanonicalRawJSON(tool.Parameters),
			Format:       CanonicalRawJSON(tool.Format),
			Execution:    tool.Execution,
			Strict:       tool.Strict,
			DeferLoading: tool.DeferLoading,
			Children:     children,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func normalizedToolSemanticKey(tool NormalizedTool) string {
	b, _ := json.Marshal(comparableTools([]NormalizedTool{tool}))
	return string(b)
}

func namespaceHeaderKey(tool NormalizedTool) string {
	clone := cloneNormalizedTool(tool, false)
	clone.Children = nil
	return normalizedToolSemanticKey(clone)
}

func CanonicalRawJSON(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(trimmed)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(trimmed)
	}
	return string(b)
}

func flattenedToolCount(tools []NormalizedTool) int {
	count := 0
	for _, tool := range tools {
		if tool.Kind == ToolKindNamespace {
			count += len(tool.Children)
			continue
		}
		count++
	}
	return count
}

func validateLoadedToolLimits(tools []NormalizedTool) error {
	for _, tool := range tools {
		if len(tool.Description) > MaxLoadedDescriptionBytes {
			return InvalidRequest("tool_search_output.tools description is too large", "input")
		}
		if len(tool.Parameters) > MaxLoadedToolSchemaBytes || len(tool.Format) > MaxLoadedToolSchemaBytes {
			return InvalidRequest("tool_search_output.tools schema is too large", "input")
		}
		if tool.Kind == ToolKindNamespace {
			if len(tool.Children) > MaxLoadedToolCount {
				return InvalidRequest("tool_search_output.tools namespace contains too many tools", "input")
			}
			if err := validateLoadedToolLimits(tool.Children); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateInstalledToolCount(tools []NormalizedTool, param string) error {
	if flattenedToolCount(tools) > MaxInstalledToolCount {
		return InvalidRequest("installed tool catalog contains too many tools", param)
	}
	return nil
}

func validateStoredCatalogSize(c ToolCatalog) error {
	b, err := json.Marshal(c.StoredDTO())
	if err != nil {
		return err
	}
	if len(b) > MaxLoadedCatalogBytes {
		return InvalidRequest("installed tool catalog is too large", "input")
	}
	return nil
}

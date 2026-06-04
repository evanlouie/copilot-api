package copilotgw

import (
	"context"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func (g *RealGateway) ListModels(ctx context.Context) ([]Model, error) {
	return g.refreshModels(ctx, false)
}
func (g *RealGateway) ValidateModel(ctx context.Context, model string) error {
	_, err := g.findModel(ctx, model)
	return err
}
func (g *RealGateway) refreshModels(ctx context.Context, force bool) ([]Model, error) {
	g.modelsMu.Lock()
	defer g.modelsMu.Unlock()
	if !force && g.models != nil && (g.modelsCacheTTL <= 0 || time.Since(g.modelsFetched) < g.modelsCacheTTL) {
		out := append([]Model(nil), g.models...)
		return out, nil
	}
	var out []Model
	if g.client.RPC != nil {
		list, err := g.client.RPC.Models.List(ctx, &rpc.ModelsListRequest{})
		if err != nil {
			return nil, err
		}
		for _, m := range list.Models {
			supportsVision, visionKnown := rpcVisionSupport(m.Capabilities.Supports)
			supportsReasoningEffort, reasoningEffortKnown := rpcReasoningEffortSupport(m.Capabilities.Supports)
			supportedReasoningEfforts := cleanReasoningEfforts(m.SupportedReasoningEfforts)
			defaultReasoningEffort := ""
			if m.DefaultReasoningEffort != nil {
				defaultReasoningEffort = cleanReasoningEffort(*m.DefaultReasoningEffort)
			}
			if len(supportedReasoningEfforts) > 0 || defaultReasoningEffort != "" {
				supportsReasoningEffort = true
				reasoningEffortKnown = true
			}
			limits := rpcTokenLimits(m.Capabilities.Limits)
			vision := rpcVisionLimits(m.Capabilities.Limits)
			meta := modelMetadata(m.Name, supportsVision, visionKnown, limits, vision)
			addReasoningMetadata(meta, supportsReasoningEffort, reasoningEffortKnown, supportedReasoningEfforts, defaultReasoningEffort)
			out = append(out, Model{ID: m.ID, Name: m.Name, Metadata: meta, Limits: limits, VisionKnown: visionKnown, SupportsVision: supportsVision, Vision: vision, ReasoningEffortKnown: reasoningEffortKnown, SupportsReasoningEffort: supportsReasoningEffort, SupportedReasoningEfforts: supportedReasoningEfforts, DefaultReasoningEffort: defaultReasoningEffort})
		}
	} else {
		models, err := g.client.ListModels(ctx)
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			supportedReasoningEfforts := cleanReasoningEfforts(m.SupportedReasoningEfforts)
			defaultReasoningEffort := cleanReasoningEffort(m.DefaultReasoningEffort)
			supportsReasoningEffort := m.Capabilities.Supports.ReasoningEffort || len(supportedReasoningEfforts) > 0 || defaultReasoningEffort != ""
			reasoningEffortKnown := true
			limits := sdkTokenLimits(m.Capabilities.Limits)
			vision := sdkVisionLimits(m.Capabilities.Limits.Vision)
			meta := modelMetadata(m.Name, m.Capabilities.Supports.Vision, true, limits, vision)
			addReasoningMetadata(meta, supportsReasoningEffort, reasoningEffortKnown, supportedReasoningEfforts, defaultReasoningEffort)
			out = append(out, Model{ID: m.ID, Name: m.Name, Metadata: meta, Limits: limits, VisionKnown: true, SupportsVision: m.Capabilities.Supports.Vision, Vision: vision, ReasoningEffortKnown: reasoningEffortKnown, SupportsReasoningEffort: supportsReasoningEffort, SupportedReasoningEfforts: supportedReasoningEfforts, DefaultReasoningEffort: defaultReasoningEffort})
		}
	}
	g.models = append([]Model(nil), out...)
	g.modelsFetched = time.Now()
	return out, nil
}
func rpcTokenLimits(limits *rpc.ModelCapabilitiesLimits) *TokenLimits {
	if limits == nil {
		return nil
	}
	out := &TokenLimits{
		MaxContextWindowTokens: cloneInt64Ptr(limits.MaxContextWindowTokens),
		MaxPromptTokens:        cloneInt64Ptr(limits.MaxPromptTokens),
		MaxOutputTokens:        cloneInt64Ptr(limits.MaxOutputTokens),
	}
	if out.empty() {
		return nil
	}
	return out
}
func rpcVisionSupport(supports *rpc.ModelCapabilitiesSupports) (bool, bool) {
	if supports == nil || supports.Vision == nil {
		return false, false
	}
	return *supports.Vision, true
}
func rpcReasoningEffortSupport(supports *rpc.ModelCapabilitiesSupports) (bool, bool) {
	if supports == nil || supports.ReasoningEffort == nil {
		return false, false
	}
	return *supports.ReasoningEffort, true
}
func rpcVisionLimits(limits *rpc.ModelCapabilitiesLimits) *VisionLimits {
	if limits == nil || limits.Vision == nil {
		return nil
	}
	return &VisionLimits{
		SupportedMediaTypes: limits.Vision.SupportedMediaTypes,
		MaxPromptImages:     limits.Vision.MaxPromptImages,
		MaxPromptImageSize:  limits.Vision.MaxPromptImageSize,
	}
}
func sdkVisionLimits(limits *copilot.ModelVisionLimits) *VisionLimits {
	if limits == nil {
		return nil
	}
	return &VisionLimits{
		SupportedMediaTypes: limits.SupportedMediaTypes,
		MaxPromptImages:     int64(limits.MaxPromptImages),
		MaxPromptImageSize:  int64(limits.MaxPromptImageSize),
	}
}
func sdkTokenLimits(limits copilot.ModelLimits) *TokenLimits {
	out := &TokenLimits{
		MaxContextWindowTokens: positiveIntPtrToInt64Ptr(limits.MaxContextWindowTokens),
		MaxPromptTokens:        intPtrToInt64Ptr(limits.MaxPromptTokens),
	}
	if out.empty() {
		return nil
	}
	return out
}
func (l *TokenLimits) empty() bool {
	return l == nil || l.MaxContextWindowTokens == nil && l.MaxPromptTokens == nil && l.MaxOutputTokens == nil
}
func cloneInt64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
func intPtrToInt64Ptr(v *int) *int64 {
	if v == nil {
		return nil
	}
	out := int64(*v)
	return &out
}

// positiveIntPtrToInt64Ptr converts an *int to *int64, treating nil or
// non-positive values as "unknown" (nil). It mirrors the historical
// suppression behavior used for context-window-style limits where 0 carries
// no meaningful signal to OpenAI clients.
func positiveIntPtrToInt64Ptr(v *int) *int64 {
	if v == nil || *v <= 0 {
		return nil
	}
	out := int64(*v)
	return &out
}
func modelMetadata(name string, supportsVision bool, visionKnown bool, limits *TokenLimits, vision *VisionLimits) map[string]any {
	meta := map[string]any{"name": name}
	capabilities := map[string]any{}
	if visionKnown {
		meta["supports_vision"] = supportsVision
		capabilities["supports"] = map[string]any{"vision": supportsVision}
	}
	if limitMeta := tokenLimitsMetadata(limits); len(limitMeta) > 0 {
		for k, v := range limitMeta {
			meta[k] = v
		}
		capabilities["limits"] = limitMeta
	}
	if len(capabilities) > 0 {
		meta["capabilities"] = capabilities
	}
	if vision != nil {
		meta["vision"] = map[string]any{
			"supported_media_types": vision.SupportedMediaTypes,
			"max_prompt_images":     vision.MaxPromptImages,
			"max_prompt_image_size": vision.MaxPromptImageSize,
		}
	}
	return meta
}
func addReasoningMetadata(meta map[string]any, supportsReasoningEffort bool, reasoningEffortKnown bool, efforts []string, defaultEffort string) {
	if reasoningEffortKnown {
		meta["supports_reasoning_effort"] = supportsReasoningEffort
		capabilities, _ := meta["capabilities"].(map[string]any)
		if capabilities == nil {
			capabilities = map[string]any{}
			meta["capabilities"] = capabilities
		}
		supports, _ := capabilities["supports"].(map[string]any)
		if supports == nil {
			supports = map[string]any{}
			capabilities["supports"] = supports
		}
		supports["reasoning_effort"] = supportsReasoningEffort
	}
	if len(efforts) > 0 {
		meta["supported_reasoning_efforts"] = append([]string(nil), efforts...)
	}
	if defaultEffort != "" {
		meta["default_reasoning_effort"] = defaultEffort
	}
}
func tokenLimitsMetadata(limits *TokenLimits) map[string]any {
	meta := map[string]any{}
	if limits == nil {
		return meta
	}
	if limits.MaxContextWindowTokens != nil {
		meta["max_context_window_tokens"] = *limits.MaxContextWindowTokens
	}
	if limits.MaxPromptTokens != nil {
		meta["max_prompt_tokens"] = *limits.MaxPromptTokens
	}
	if limits.MaxOutputTokens != nil {
		meta["max_output_tokens"] = *limits.MaxOutputTokens
	}
	return meta
}

var reasoningEffortRanks = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
}

func (g *RealGateway) ResolveReasoningEffort(ctx context.Context, model, requestedEffort, defaultEffort string) (string, error) {
	return g.effectiveReasoningEffort(ctx, model, requestedEffort, defaultEffort)
}

func (g *RealGateway) requestReasoningEffort(ctx context.Context, model, requestedEffort, defaultEffort, resolvedEffort string, resolved bool) (string, error) {
	if resolved {
		return cleanReasoningEffort(resolvedEffort), nil
	}
	return g.effectiveReasoningEffort(ctx, model, requestedEffort, defaultEffort)
}

func (g *RealGateway) effectiveReasoningEffort(ctx context.Context, model, requestedEffort, defaultEffort string) (string, error) {
	requestedEffort = cleanReasoningEffort(requestedEffort)
	if requestedEffort != "" {
		return requestedEffort, nil
	}
	defaultEffort = cleanReasoningEffort(defaultEffort)
	if defaultEffort == "" {
		defaultEffort = cleanReasoningEffort(g.cfg.DefaultReasoningEffort)
	}
	if defaultEffort == "" {
		return "", nil
	}
	modelInfo, err := g.findModel(ctx, model)
	if err != nil {
		return "", err
	}
	return closestReasoningEffort(defaultEffort, modelInfo), nil
}

func closestReasoningEffort(defaultEffort string, model Model) string {
	defaultEffort = cleanReasoningEffort(defaultEffort)
	if defaultEffort == "" {
		return ""
	}
	if len(model.SupportedReasoningEfforts) > 0 {
		for _, effort := range model.SupportedReasoningEfforts {
			if cleanReasoningEffort(effort) == defaultEffort {
				return cleanReasoningEffort(effort)
			}
		}
		defaultRank, ok := reasoningEffortRanks[defaultEffort]
		if !ok {
			return ""
		}
		bestEffort := ""
		bestDistance := 1 << 30
		bestRank := 1 << 30
		for _, effort := range model.SupportedReasoningEfforts {
			cleaned := cleanReasoningEffort(effort)
			rank, ok := reasoningEffortRanks[cleaned]
			if !ok {
				continue
			}
			distance := abs(rank - defaultRank)
			if distance < bestDistance || distance == bestDistance && rank < bestRank {
				bestEffort = cleaned
				bestDistance = distance
				bestRank = rank
			}
		}
		return bestEffort
	}
	if model.ReasoningEffortKnown && !model.SupportsReasoningEffort {
		return ""
	}
	if defaultEffort == "none" {
		return ""
	}
	if model.DefaultReasoningEffort != "" {
		return cleanReasoningEffort(model.DefaultReasoningEffort)
	}
	if model.ReasoningEffortKnown && model.SupportsReasoningEffort {
		return defaultEffort
	}
	return ""
}

func cleanReasoningEfforts(efforts []string) []string {
	out := make([]string, 0, len(efforts))
	seen := map[string]struct{}{}
	for _, effort := range efforts {
		cleaned := cleanReasoningEffort(effort)
		if cleaned == "" {
			continue
		}
		if _, ok := seen[cleaned]; ok {
			continue
		}
		seen[cleaned] = struct{}{}
		out = append(out, cleaned)
	}
	return out
}

func cleanReasoningEffort(effort string) string {
	return strings.ToLower(strings.TrimSpace(effort))
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

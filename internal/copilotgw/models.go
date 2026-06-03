package copilotgw

import (
	"context"
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
			limits := rpcTokenLimits(m.Capabilities.Limits)
			vision := rpcVisionLimits(m.Capabilities.Limits)
			meta := modelMetadata(m.Name, supportsVision, visionKnown, limits, vision)
			if len(m.SupportedReasoningEfforts) > 0 {
				meta["supported_reasoning_efforts"] = m.SupportedReasoningEfforts
			}
			if m.DefaultReasoningEffort != nil {
				meta["default_reasoning_effort"] = *m.DefaultReasoningEffort
			}
			out = append(out, Model{ID: m.ID, Name: m.Name, Metadata: meta, Limits: limits, VisionKnown: visionKnown, SupportsVision: supportsVision, Vision: vision})
		}
	} else {
		models, err := g.client.ListModels(ctx)
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			limits := sdkTokenLimits(m.Capabilities.Limits)
			vision := sdkVisionLimits(m.Capabilities.Limits.Vision)
			out = append(out, Model{ID: m.ID, Name: m.Name, Metadata: modelMetadata(m.Name, m.Capabilities.Supports.Vision, true, limits, vision), Limits: limits, VisionKnown: true, SupportsVision: m.Capabilities.Supports.Vision, Vision: vision})
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
		MaxContextWindowTokens: positiveIntToInt64Ptr(limits.MaxContextWindowTokens),
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
func positiveIntToInt64Ptr(v int) *int64 {
	if v <= 0 {
		return nil
	}
	out := int64(v)
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

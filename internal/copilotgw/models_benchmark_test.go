package copilotgw

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// benchmarkModelCatalog mirrors the shape of a real Copilot subscription
// listing: a few dozen models, each carrying the nested capability metadata the
// RPC response fills in. The nesting matters because cloneModels recurses
// through Metadata, so a flat map would understate the per-lookup cost.
func benchmarkModelCatalog(n int) []Model {
	models := make([]Model, n)
	for i := range models {
		maxContext := int64(128_000)
		maxPrompt := int64(120_000)
		maxOutput := int64(16_000)
		models[i] = Model{
			ID:   fmt.Sprintf("model-%02d", i),
			Name: fmt.Sprintf("Benchmark Model %02d", i),
			Metadata: map[string]any{
				"vendor": "benchmark",
				"family": fmt.Sprintf("family-%02d", i%4),
				"policy": map[string]any{"state": "enabled", "terms": "none"},
				"billing": map[string]any{
					"is_premium": true,
					"multiplier": 1,
				},
				"capabilities": map[string]any{
					"type": "chat",
					"supports": map[string]any{
						"streaming":           true,
						"tool_calls":          true,
						"parallel_tool_calls": true,
						"vision":              true,
						"reasoning_effort":    true,
					},
					"limits": map[string]any{
						"max_context_window_tokens": maxContext,
						"max_prompt_tokens":         maxPrompt,
						"max_output_tokens":         maxOutput,
						"vision": map[string]any{
							"max_prompt_images":     int64(5),
							"max_prompt_image_size": int64(3_145_728),
							"supported_media_types": []any{"image/png", "image/jpeg", "image/webp"},
						},
					},
				},
				"tags": []any{"chat", "tools", "vision"},
			},
			Limits:                    &TokenLimits{MaxContextWindowTokens: &maxContext, MaxPromptTokens: &maxPrompt, MaxOutputTokens: &maxOutput},
			VisionKnown:               true,
			SupportsVision:            true,
			Vision:                    &VisionLimits{SupportedMediaTypes: []string{"image/png", "image/jpeg", "image/webp"}, MaxPromptImages: 5, MaxPromptImageSize: 3_145_728},
			ReasoningEffortKnown:      true,
			SupportsReasoningEffort:   true,
			SupportedReasoningEfforts: []string{"low", "medium", "high"},
			DefaultReasoningEffort:    "medium",
		}
	}
	return models
}

const benchmarkModelCatalogSize = 48

func benchmarkModelGateway() *RealGateway {
	return &RealGateway{modelCache: &modelCache{models: benchmarkModelCatalog(benchmarkModelCatalogSize), fetched: time.Now(), ttl: time.Hour}}
}

// BenchmarkFindModelCacheHit measures the cost of answering "does model X
// exist, and what are its capabilities" against a warm cache. This runs at
// least twice per request (ValidateModel and requestReasoningEffort) and once
// more per message carrying an image.
func BenchmarkFindModelCacheHit(b *testing.B) {
	gw := benchmarkModelGateway()
	ctx := context.Background()
	id := fmt.Sprintf("model-%02d", benchmarkModelCatalogSize/2)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := gw.findModel(ctx, id); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFindModelCacheHitLast pins the worst case for the linear scan: the
// requested model is the last entry in the catalog.
func BenchmarkFindModelCacheHitLast(b *testing.B) {
	gw := benchmarkModelGateway()
	ctx := context.Background()
	id := fmt.Sprintf("model-%02d", benchmarkModelCatalogSize-1)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := gw.findModel(ctx, id); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkModelLookupsPerRequest is the per-request model work for a chat
// completion carrying one image: ValidateModel, requestReasoningEffort, and one
// per-image capability check.
func BenchmarkModelLookupsPerRequest(b *testing.B) {
	gw := benchmarkModelGateway()
	gw.cfg.DefaultReasoningEffort = "medium"
	ctx := context.Background()
	id := fmt.Sprintf("model-%02d", benchmarkModelCatalogSize/2)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := gw.ValidateModel(ctx, id); err != nil {
			b.Fatal(err)
		}
		if _, err := gw.requestReasoningEffort(ctx, id, "", "", "", false); err != nil {
			b.Fatal(err)
		}
		if _, err := gw.findModel(ctx, id); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkListModels covers the caller that genuinely needs the whole catalog
// cloned, so a lookup-path optimization cannot be mistaken for a change here.
func BenchmarkListModels(b *testing.B) {
	gw := benchmarkModelGateway()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		models, err := gw.ListModels(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if len(models) != benchmarkModelCatalogSize {
			b.Fatalf("ListModels returned %d models", len(models))
		}
	}
}

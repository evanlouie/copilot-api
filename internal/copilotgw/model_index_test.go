package copilotgw

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// linearFindModel is the lookup findModel used to perform: a scan of the whole
// catalog returning the first entry with a matching id. It is the reference the
// id index has to stay equivalent to.
func linearFindModel(models []Model, id string) (Model, bool) {
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// assertIndexMatchesModels checks the invariant that makes the index safe: for
// every id, resolving through the index must give the same answer as scanning
// the catalog the index was built from. An index assigned separately from
// cache.models would satisfy this only until the two drifted apart.
func assertIndexMatchesModels(t *testing.T, cache *modelCache, context string) {
	t.Helper()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.index) > len(cache.models) {
		t.Fatalf("%s: index has %d entries for %d models", context, len(cache.index), len(cache.models))
	}
	seen := map[string]struct{}{}
	for _, model := range cache.models {
		if _, dup := seen[model.ID]; dup {
			continue
		}
		seen[model.ID] = struct{}{}
		got, ok := cache.lookupModelLocked(model.ID)
		if !ok {
			t.Fatalf("%s: cached model %q is not reachable through the id index", context, model.ID)
		}
		want, _ := linearFindModel(cache.models, model.ID)
		if got.ID != want.ID || got.Name != want.Name {
			t.Fatalf("%s: index resolved %q to %#v, linear scan found %#v", context, model.ID, got, want)
		}
	}
	for id := range cache.index {
		if _, ok := linearFindModel(cache.models, id); !ok {
			t.Fatalf("%s: index still names %q, which is not in the catalog", context, id)
		}
	}
	for _, id := range []string{"", "absent", "MODEL-00"} {
		_, indexed := cache.lookupModelLocked(id)
		_, scanned := linearFindModel(cache.models, id)
		if indexed != scanned {
			t.Fatalf("%s: lookup of %q = %v, linear scan = %v", context, id, indexed, scanned)
		}
	}
}

func catalog(ids ...string) []Model {
	models := make([]Model, len(ids))
	for i, id := range ids {
		models[i] = Model{ID: id, Name: "name-" + id, Metadata: map[string]any{"id": id}}
	}
	return models
}

// TestModelIndexStaysConsistentAcrossRefreshPaths drives every path that can
// replace cache.models and checks the index afterwards. A rebuild missed on any
// one of them lets a lookup answer from a stale index against fresh models.
func TestModelIndexStaysConsistentAcrossRefreshPaths(t *testing.T) {
	t.Parallel()
	var current []Model
	var fetchErr error
	gw := &RealGateway{
		modelCache: newModelCache(20 * time.Millisecond),
		modelsFetcher: func(context.Context) ([]Model, error) {
			if fetchErr != nil {
				return nil, fetchErr
			}
			return append([]Model(nil), current...), nil
		},
	}
	ctx := context.Background()

	// Cold cache, caller issues its own fetch.
	current = catalog("a", "b")
	if _, err := gw.ListModels(ctx); err != nil {
		t.Fatal(err)
	}
	assertIndexMatchesModels(t, gw.modelCache, "cold fetch")

	// Forced refresh onto a different catalog: the ids that vanished must stop
	// resolving, and the new ones must start.
	current = catalog("b", "c", "d")
	if _, err := gw.refreshModels(ctx, true); err != nil {
		t.Fatal(err)
	}
	assertIndexMatchesModels(t, gw.modelCache, "forced refresh")
	if _, ok := gw.lookupModel("a"); ok {
		t.Fatal("a model dropped by a forced refresh is still reachable through the id index")
	}
	if _, ok := gw.lookupModel("d"); !ok {
		t.Fatal("a model added by a forced refresh is not reachable through the id index")
	}

	// A failed refresh must leave both the catalog and the index alone.
	fetchErr = errors.New("upstream down")
	if _, err := gw.refreshModels(ctx, true); err == nil {
		t.Fatal("forced refresh did not report the upstream failure")
	}
	assertIndexMatchesModels(t, gw.modelCache, "failed refresh")
	if _, ok := gw.lookupModel("c"); !ok {
		t.Fatal("a failed refresh dropped a model from the id index")
	}
	fetchErr = nil

	// Stale cache: the caller is served immediately while a background refresh
	// swaps the catalog underneath it.
	current = catalog("e", "f")
	time.Sleep(40 * time.Millisecond)
	if _, err := gw.ListModels(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := gw.lookupModel("e"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh never published its catalog to the id index")
		}
		time.Sleep(2 * time.Millisecond)
	}
	assertIndexMatchesModels(t, gw.modelCache, "background refresh")
}

// TestModelIndexResolvesDuplicateIDsLikeTheScanItReplaced pins the tie-break.
// The linear scan returned the first match; so must the index.
func TestModelIndexResolvesDuplicateIDsLikeTheScanItReplaced(t *testing.T) {
	t.Parallel()
	models := []Model{{ID: "dup", Name: "first"}, {ID: "other"}, {ID: "dup", Name: "second"}}
	cache := newModelCacheWithModels(models, time.Hour)
	got, ok := cache.lookupModelLocked("dup")
	if !ok {
		t.Fatal("duplicate id is not reachable")
	}
	want, _ := linearFindModel(models, "dup")
	if got.Name != want.Name {
		t.Fatalf("lookup resolved duplicate id to %q, linear scan found %q", got.Name, want.Name)
	}
}

// TestLookupModelReturnsAnIndependentCopy keeps the deep clone that callers
// relied on when findModel handed out an entry from a freshly cloned catalog.
func TestLookupModelReturnsAnIndependentCopy(t *testing.T) {
	t.Parallel()
	limit := int64(128_000)
	gw := &RealGateway{modelCache: newModelCacheWithModels([]Model{{
		ID:                        "gpt-5",
		Metadata:                  map[string]any{"policy": map[string]any{"state": "enabled"}},
		Limits:                    &TokenLimits{MaxContextWindowTokens: &limit},
		Vision:                    &VisionLimits{SupportedMediaTypes: []string{"image/png"}},
		SupportedReasoningEfforts: []string{"low", "high"},
	}}, time.Hour)}
	first, ok := gw.lookupModel("gpt-5")
	if !ok {
		t.Fatal("model not found")
	}
	first.Metadata["policy"].(map[string]any)["state"] = "disabled"
	first.Metadata["injected"] = true
	first.Vision.SupportedMediaTypes[0] = "image/gif"
	first.SupportedReasoningEfforts[0] = "mutated"
	*first.Limits.MaxContextWindowTokens = 1

	second, ok := gw.lookupModel("gpt-5")
	if !ok {
		t.Fatal("model not found on the second lookup")
	}
	if second.Metadata["policy"].(map[string]any)["state"] != "enabled" {
		t.Fatal("mutating a looked-up model's nested metadata reached the cache")
	}
	if _, injected := second.Metadata["injected"]; injected {
		t.Fatal("mutating a looked-up model's metadata reached the cache")
	}
	if second.Vision.SupportedMediaTypes[0] != "image/png" {
		t.Fatal("mutating a looked-up model's vision media types reached the cache")
	}
	if second.SupportedReasoningEfforts[0] != "low" {
		t.Fatal("mutating a looked-up model's reasoning efforts reached the cache")
	}
	if *second.Limits.MaxContextWindowTokens != 128_000 {
		t.Fatal("mutating a looked-up model's limits reached the cache")
	}
}

// TestFindModelAgreesWithLinearScan checks the replacement end to end for every
// id in a catalog plus the not-found and empty-id cases findModel handles
// specially.
func TestFindModelAgreesWithLinearScan(t *testing.T) {
	t.Parallel()
	models := catalog("a", "b", "c")
	gw := &RealGateway{
		modelCache:    newModelCacheWithModels(models, time.Hour),
		modelsFetcher: func(context.Context) ([]Model, error) { return append([]Model(nil), models...), nil },
	}
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c", "absent", "A", " a"} {
		got, err := gw.findModel(ctx, id)
		want, ok := linearFindModel(models, id)
		if ok {
			if err != nil {
				t.Fatalf("findModel(%q) = %v, want %#v", id, err, want)
			}
			if got.ID != want.ID || got.Name != want.Name {
				t.Fatalf("findModel(%q) = %#v, want %#v", id, got, want)
			}
			continue
		}
		if err == nil {
			t.Fatalf("findModel(%q) = %#v, want not found", id, got)
		}
		if msg := err.Error(); msg != fmt.Sprintf("model not found: %s", id) {
			t.Fatalf("findModel(%q) error = %q", id, msg)
		}
	}
	if _, err := gw.findModel(ctx, ""); err == nil || err.Error() != "model is required" {
		t.Fatalf("findModel(\"\") error = %v, want the empty-id validation error", err)
	}
}

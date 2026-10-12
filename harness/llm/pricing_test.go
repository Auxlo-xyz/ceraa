package llm

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCatalogClientRefreshesOncePerDay(t *testing.T) {
	var calls atomic.Int32
	price := "0.0000001"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := catalogResponse{Data: []catalogModel{{
			ID:            "test/model",
			Name:          "Test Model",
			ContextWindow: 200000,
			MaxTokens:     4096,
			Pricing: ModelPricing{
				Input:  TokenPrice{Base: price},
				Output: TokenPrice{Base: "0.0000002"},
			},
		}}}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := NewCatalogClient(server.URL, t.TempDir()+"/catalog.json", 24*time.Hour)
	client.Now = func() time.Time { return now }
	first, err := client.Lookup(context.Background(), "test/model")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !first.PricingCurrent || !first.PricingUsable() {
		t.Fatalf("unexpected first lookup: calls=%d metadata=%+v", calls.Load(), first)
	}
	second, err := client.Lookup(context.Background(), "test/model")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || second.PricingFetchedAt != first.PricingFetchedAt {
		t.Fatal("fresh catalog was not reused")
	}
	price = "0.0000003"
	now = now.Add(24*time.Hour + time.Minute)
	third, err := client.Lookup(context.Background(), "test/model")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || third.PricingFetchedAt.Equal(first.PricingFetchedAt) {
		t.Fatal("stale catalog was not refreshed")
	}
}

func TestCatalogClientRefusesStalePricing(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			payload := catalogResponse{Data: []catalogModel{{
				ID: "test/model",
				Pricing: ModelPricing{
					Input:  TokenPrice{Base: "0.0000001"},
					Output: TokenPrice{Base: "0.0000002"},
				},
			}}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := NewCatalogClient(server.URL, t.TempDir()+"/catalog.json", time.Hour)
	client.Now = func() time.Time { return now }
	if _, err := client.Lookup(context.Background(), "test/model"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	metadata, err := client.Lookup(context.Background(), "test/model")
	if err == nil {
		t.Fatal("expected refresh failure")
	}
	if metadata.PricingStatus(err) != "stale" {
		t.Fatalf("unexpected pricing status: %q", metadata.PricingStatus(err))
	}
	if _, ok := metadata.Cost(UsageInfo{InputTokens: 10, OutputTokens: 2}); ok {
		t.Fatal("stale pricing was used for cost")
	}
}

func TestCatalogClientParsesOpenRouterSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{"data": []map[string]any{{
			"id":                   "poolside/laguna-xs-2.1",
			"name":                 "Poolside: Laguna XS 2.1",
			"context_length":       262144,
			"supported_parameters": []string{"reasoning"},
			"pricing": map[string]any{
				"prompt":           "0.00000006",
				"completion":       "0.00000012",
				"input_cache_read": "0.00000003",
			},
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()
	client := NewCatalogClient(server.URL, t.TempDir()+"/catalog.json", 24*time.Hour)
	client.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	metadata, err := client.Lookup(context.Background(), "poolside/laguna-xs-2.1")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ContextWindow != 262144 || metadata.Pricing.Input.Base != "0.00000006" || metadata.Pricing.Output.Base != "0.00000012" {
		t.Fatalf("unexpected OpenRouter metadata: %+v", metadata)
	}
	if len(metadata.ReasoningOptions) != 1 || metadata.ReasoningOptions[0] != "supported:reasoning" {
		t.Fatalf("unexpected reasoning metadata: %+v", metadata.ReasoningOptions)
	}
	cost, ok := metadata.Cost(UsageInfo{InputTokens: 100, OutputTokens: 10})
	if !ok || math.Abs(cost.InputUSD-0.000006) > 1e-12 || math.Abs(cost.OutputUSD-0.0000012) > 1e-12 {
		t.Fatalf("unexpected OpenRouter cost: %+v", cost)
	}
}

func TestModelPricingUsesTiers(t *testing.T) {
	metadata := ModelMetadata{
		ID:             "test/model",
		PricingCurrent: true,
		Pricing: ModelPricing{
			Input:  TokenPrice{Base: "1"},
			Output: TokenPrice{Base: "2"},
			InputTiers: []PriceTier{
				{Cost: "1", Min: 0, Max: 50},
				{Cost: "3", Min: 51},
			},
		},
	}
	cost, ok := metadata.Cost(UsageInfo{InputTokens: 100, OutputTokens: 10})
	if !ok || cost.InputUSD != 300 || cost.OutputUSD != 20 {
		t.Fatalf("unexpected tiered cost: %+v", cost)
	}
}

func TestModelMetadataCalculatesBillingTypes(t *testing.T) {
	metadata := ModelMetadata{
		ID:             "test/model",
		PricingCurrent: true,
		Pricing: ModelPricing{
			Input:      TokenPrice{Base: "2"},
			Output:     TokenPrice{Base: "3"},
			CacheRead:  TokenPrice{Base: "1"},
			CacheWrite: TokenPrice{Base: "4"},
		},
	}
	cost, ok := metadata.Cost(UsageInfo{InputTokens: 100, CachedInputTokens: 40, CacheWriteTokens: 10, OutputTokens: 20})
	if !ok {
		t.Fatal("expected known cost")
	}
	if cost.InputUSD != 120 || cost.CacheReadUSD != 40 || cost.OutputUSD != 60 || cost.CacheWriteUSD != 40 || cost.TotalUSD != 260 {
		t.Fatalf("unexpected cost: %+v", cost)
	}
}

// Providers report bare model names; the catalog is keyed by namespace/name.
// Every cost this harness printed was missing because the two never met.
func TestLookupResolvesABareNameToItsNamespacedEntry(t *testing.T) {
	client := catalogWith(t, map[string]ModelMetadata{
		"openai/gpt-5.6-luna": {Pricing: ModelPricing{Input: TokenPrice{Base: "1.75"}, Output: TokenPrice{Base: "14"}}},
	})
	metadata, err := client.Lookup(context.Background(), "gpt-5.6-luna")
	if err != nil {
		t.Fatalf("bare name did not resolve: %v", err)
	}
	cost, ok := metadata.Cost(UsageInfo{InputTokens: 1000, OutputTokens: 100})
	if !ok {
		t.Fatal("the resolved entry produced no cost")
	}
	if cost.TotalUSD <= 0 {
		t.Fatalf("cost = %v, want a positive figure", cost)
	}
}

// Two entries can share a bare name. A price guessed between them is worse than
// no price, so ambiguity has to stay a miss.
func TestLookupRefusesAnAmbiguousBareName(t *testing.T) {
	client := catalogWith(t, map[string]ModelMetadata{
		"openai/shared": {},
		"azure/shared":  {},
	})
	if _, err := client.Lookup(context.Background(), "shared"); err == nil {
		t.Fatal("an ambiguous bare name resolved to a price")
	}
}

// An exact hit still wins, including when some other entry shares the bare name.
func TestLookupPrefersTheExactKey(t *testing.T) {
	client := catalogWith(t, map[string]ModelMetadata{
		"shared":       {Name: "the exact one"},
		"azure/shared": {},
	})
	metadata, err := client.Lookup(context.Background(), "shared")
	if err != nil || metadata.Name != "the exact one" {
		t.Fatalf("exact key did not win: %q %v", metadata.Name, err)
	}
}

func TestLookupStillMissesAnUnknownModel(t *testing.T) {
	client := catalogWith(t, map[string]ModelMetadata{"openai/gpt-5.6-luna": {}})
	if _, err := client.Lookup(context.Background(), "no-such-model"); err == nil {
		t.Fatal("an unknown model resolved")
	}
}

// catalogWith builds a client whose cache is already populated, so a test can
// exercise lookup without standing up a server.
func catalogWith(t *testing.T, models map[string]ModelMetadata) *CatalogClient {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := NewCatalogClient("", t.TempDir()+"/catalog.json", 24*time.Hour)
	client.Now = func() time.Time { return now }
	client.cache = &modelCatalogCache{FetchedAt: now, Models: models}
	return client
}

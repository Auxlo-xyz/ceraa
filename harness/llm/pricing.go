package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultModelCatalogURL = "https://openrouter.ai/api/v1/models"

var ErrModelNotFound = errors.New("model not found in OpenRouter model catalog")

type PriceTier struct {
	Cost string `json:"cost"`
	Min  int    `json:"min"`
	Max  int    `json:"max,omitempty"`
}

type TokenPrice struct {
	Base  string      `json:"base,omitempty"`
	Tiers []PriceTier `json:"tiers,omitempty"`
}

func (p TokenPrice) MarshalJSON() ([]byte, error) {
	if len(p.Tiers) == 0 {
		return json.Marshal(p.Base)
	}
	return json.Marshal(struct {
		Cost  string      `json:"cost"`
		Tiers []PriceTier `json:"tiers"`
	}{Cost: p.Base, Tiers: p.Tiers})
}

func (p *TokenPrice) UnmarshalJSON(data []byte) error {
	var direct string
	if err := json.Unmarshal(data, &direct); err == nil {
		p.Base = direct
		p.Tiers = nil
		return nil
	}
	var object struct {
		Cost  string      `json:"cost"`
		Tiers []PriceTier `json:"tiers"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	p.Base = object.Cost
	p.Tiers = object.Tiers
	return nil
}

func (p TokenPrice) rate(tokens int) (float64, bool) {
	if len(p.Tiers) > 0 {
		var selected *PriceTier
		for i := range p.Tiers {
			tier := &p.Tiers[i]
			if tokens >= tier.Min && (tier.Max == 0 || tokens <= tier.Max) {
				selected = tier
			}
		}
		if selected == nil && tokens >= p.Tiers[len(p.Tiers)-1].Min {
			selected = &p.Tiers[len(p.Tiers)-1]
		}
		if selected != nil && selected.Cost != "" {
			value, err := strconv.ParseFloat(selected.Cost, 64)
			if err == nil {
				return value, true
			}
		}
	}
	if p.Base == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(p.Base, 64)
	return value, err == nil
}

type ModelPricing struct {
	Input            TokenPrice  `json:"input,omitempty"`
	Output           TokenPrice  `json:"output,omitempty"`
	CacheRead        TokenPrice  `json:"input_cache_read,omitempty"`
	CacheWrite       TokenPrice  `json:"input_cache_write,omitempty"`
	InputTiers       []PriceTier `json:"input_tiers,omitempty"`
	OutputTiers      []PriceTier `json:"output_tiers,omitempty"`
	CacheReadTiers   []PriceTier `json:"input_cache_read_tiers,omitempty"`
	CacheWriteTiers  []PriceTier `json:"input_cache_write_tiers,omitempty"`
	VariesByProvider bool        `json:"varies_by_provider,omitempty"`
}

func (p ModelPricing) inputPrice() TokenPrice {
	if len(p.Input.Tiers) == 0 {
		p.Input.Tiers = p.InputTiers
	}
	return p.Input
}

func (p ModelPricing) outputPrice() TokenPrice {
	if len(p.Output.Tiers) == 0 {
		p.Output.Tiers = p.OutputTiers
	}
	return p.Output
}

func (p ModelPricing) cacheReadPrice() TokenPrice {
	if len(p.CacheRead.Tiers) == 0 {
		p.CacheRead.Tiers = p.CacheReadTiers
	}
	return p.CacheRead
}

func (p ModelPricing) cacheWritePrice() TokenPrice {
	if len(p.CacheWrite.Tiers) == 0 {
		p.CacheWrite.Tiers = p.CacheWriteTiers
	}
	return p.CacheWrite
}

func (p *ModelPricing) UnmarshalJSON(data []byte) error {
	var raw struct {
		Input            TokenPrice  `json:"input"`
		Output           TokenPrice  `json:"output"`
		Prompt           TokenPrice  `json:"prompt"`
		Completion       TokenPrice  `json:"completion"`
		CacheRead        TokenPrice  `json:"input_cache_read"`
		CacheWrite       TokenPrice  `json:"input_cache_write"`
		InputTiers       []PriceTier `json:"input_tiers"`
		OutputTiers      []PriceTier `json:"output_tiers"`
		CacheReadTiers   []PriceTier `json:"input_cache_read_tiers"`
		CacheWriteTiers  []PriceTier `json:"input_cache_write_tiers"`
		VariesByProvider bool        `json:"varies_by_provider"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Input.Base == "" && len(raw.Input.Tiers) == 0 {
		raw.Input = raw.Prompt
	}
	if raw.Output.Base == "" && len(raw.Output.Tiers) == 0 {
		raw.Output = raw.Completion
	}
	p.Input = raw.Input
	p.Output = raw.Output
	p.CacheRead = raw.CacheRead
	p.CacheWrite = raw.CacheWrite
	p.InputTiers = raw.InputTiers
	p.OutputTiers = raw.OutputTiers
	p.CacheReadTiers = raw.CacheReadTiers
	p.CacheWriteTiers = raw.CacheWriteTiers
	p.VariesByProvider = raw.VariesByProvider
	return nil
}

type ModelMetadata struct {
	ID               string       `json:"id"`
	Name             string       `json:"name,omitempty"`
	OwnedBy          string       `json:"owned_by,omitempty"`
	ContextWindow    int          `json:"context_window,omitempty"`
	MaxTokens        int          `json:"max_tokens,omitempty"`
	ReasoningOptions []string     `json:"reasoning_options,omitempty"`
	Pricing          ModelPricing `json:"pricing"`
	PricingCurrent   bool         `json:"-"`
	PricingFetchedAt time.Time    `json:"-"`
}

func (m ModelMetadata) PricingUsable() bool {
	if !m.PricingCurrent || m.Pricing.VariesByProvider {
		return false
	}
	_, inputOK := m.Pricing.inputPrice().rate(0)
	_, outputOK := m.Pricing.outputPrice().rate(0)
	return inputOK && outputOK
}

func (m ModelMetadata) PricingStatus(err error) string {
	if err != nil {
		if errors.Is(err, ErrModelNotFound) {
			return "model_not_found"
		}
		if m.ID != "" && !m.PricingCurrent {
			return "stale"
		}
		return "unavailable"
	}
	if !m.PricingCurrent {
		return "stale"
	}
	if m.Pricing.VariesByProvider {
		return "varies_by_provider"
	}
	if !m.PricingUsable() {
		return "price_unavailable"
	}
	return "current"
}

type CostInfo struct {
	InputUSD      float64 `json:"input_usd,omitempty"`
	OutputUSD     float64 `json:"output_usd,omitempty"`
	CacheReadUSD  float64 `json:"cache_read_usd,omitempty"`
	CacheWriteUSD float64 `json:"cache_write_usd,omitempty"`
	TotalUSD      float64 `json:"total_usd,omitempty"`
	Known         bool    `json:"known"`
}

func (c *CostInfo) Add(other CostInfo) {
	if !other.Known {
		return
	}
	c.InputUSD += other.InputUSD
	c.OutputUSD += other.OutputUSD
	c.CacheReadUSD += other.CacheReadUSD
	c.CacheWriteUSD += other.CacheWriteUSD
	c.TotalUSD += other.TotalUSD
	c.Known = true
}

func (m ModelMetadata) Cost(usage UsageInfo) (CostInfo, bool) {
	if !m.PricingUsable() {
		return CostInfo{}, false
	}
	usage = usage.normalized()
	inputRate, inputOK := m.Pricing.inputPrice().rate(usage.InputTokens)
	outputRate, outputOK := m.Pricing.outputPrice().rate(usage.InputTokens)
	if !inputOK || !outputOK {
		return CostInfo{}, false
	}
	cost := CostInfo{
		InputUSD:  float64(usage.UncachedInputTokens()) * inputRate,
		OutputUSD: float64(usage.OutputTokens) * outputRate,
	}
	if usage.CachedInputTokens > 0 {
		rate, ok := m.Pricing.cacheReadPrice().rate(usage.InputTokens)
		if !ok {
			rate = inputRate
		}
		cost.CacheReadUSD = float64(usage.CachedInputTokens) * rate
	}
	if usage.CacheWriteTokens > 0 {
		rate, ok := m.Pricing.cacheWritePrice().rate(usage.InputTokens)
		if !ok {
			rate = inputRate
		}
		cost.CacheWriteUSD = float64(usage.CacheWriteTokens) * rate
	}
	cost.TotalUSD = cost.InputUSD + cost.OutputUSD + cost.CacheReadUSD + cost.CacheWriteUSD
	cost.Known = true
	return cost, true
}

type ReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values,omitempty"`
	Min    int      `json:"min,omitempty"`
	Max    int      `json:"max,omitempty"`
}

type catalogModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	OwnedBy       string `json:"owned_by"`
	ContextWindow int    `json:"context_window"`
	MaxTokens     int    `json:"max_tokens"`
	ContextLength int    `json:"context_length"`
	TopProvider   struct {
		ContextLength       int `json:"context_length"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	ReasoningOptions    []ReasoningOption `json:"reasoning_options"`
	SupportedParameters []string          `json:"supported_parameters"`
	Pricing             ModelPricing      `json:"pricing"`
}

type catalogResponse struct {
	Data []catalogModel `json:"data"`
}

type modelCatalogCache struct {
	Source    string                   `json:"source,omitempty"`
	FetchedAt time.Time                `json:"fetched_at"`
	Models    map[string]ModelMetadata `json:"models"`
}

type CatalogClient struct {
	mu              sync.Mutex
	URL             string
	CachePath       string
	RefreshInterval time.Duration
	HTTPClient      *http.Client
	Now             func() time.Time
	cache           *modelCatalogCache
}

func DefaultModelCatalogCachePath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "sooraa", "model-catalog.json")
}

func NewCatalogClient(url, cachePath string, refreshInterval time.Duration) *CatalogClient {
	if url == "" {
		url = DefaultModelCatalogURL
	}
	if cachePath == "" {
		cachePath = DefaultModelCatalogCachePath()
	}
	if refreshInterval <= 0 {
		refreshInterval = 24 * time.Hour
	}
	return &CatalogClient{
		URL:             url,
		CachePath:       cachePath,
		RefreshInterval: refreshInterval,
		HTTPClient:      &http.Client{Timeout: 30 * time.Second},
		Now:             time.Now,
	}
}

func (c *CatalogClient) List(ctx context.Context) ([]ModelMetadata, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache, _ = c.readCache()
	}
	if c.cache != nil && c.fresh() {
		return c.modelsLocked(), nil
	}
	if err := c.refreshLocked(ctx); err != nil {
		if c.cache != nil {
			return c.modelsLocked(), err
		}
		return nil, err
	}
	return c.modelsLocked(), nil
}

func (c *CatalogClient) modelsLocked() []ModelMetadata {
	models := make([]ModelMetadata, 0, len(c.cache.Models))
	for _, model := range c.cache.Models {
		models = append(models, model)
	}
	return models
}

func (c *CatalogClient) Lookup(ctx context.Context, model string) (ModelMetadata, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache, _ = c.readCache()
	}
	if c.cache != nil && c.fresh() {
		return c.lookupLocked(model)
	}
	if err := c.refreshLocked(ctx); err != nil {
		if c.cache != nil {
			if metadata, lookupErr := c.lookupLocked(model); lookupErr == nil {
				metadata.PricingCurrent = false
				return metadata, fmt.Errorf("pricing catalog refresh failed: %w", err)
			}
		}
		return ModelMetadata{}, fmt.Errorf("pricing catalog unavailable: %w", err)
	}
	return c.lookupLocked(model)
}

func (c *CatalogClient) lookupLocked(model string) (ModelMetadata, error) {
	if c.cache == nil {
		return ModelMetadata{}, ErrModelNotFound
	}
	metadata, ok := c.cache.Models[model]
	if !ok {
		return ModelMetadata{}, fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	metadata.PricingCurrent = c.fresh()
	metadata.PricingFetchedAt = c.cache.FetchedAt
	return metadata, nil
}

func (c *CatalogClient) fresh() bool {
	return c.cache != nil && c.now().Sub(c.cache.FetchedAt) >= 0 && c.now().Sub(c.cache.FetchedAt) < c.RefreshInterval
}

func (c *CatalogClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *CatalogClient) refreshLocked(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("catalog request %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload catalogResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10*1024*1024)).Decode(&payload); err != nil {
		return err
	}
	cache := &modelCatalogCache{Source: c.URL, FetchedAt: c.now(), Models: make(map[string]ModelMetadata, len(payload.Data))}
	for _, model := range payload.Data {
		contextWindow := model.ContextWindow
		if contextWindow == 0 {
			contextWindow = model.ContextLength
		}
		if contextWindow == 0 {
			contextWindow = model.TopProvider.ContextLength
		}
		maxTokens := model.MaxTokens
		if maxTokens == 0 {
			maxTokens = model.TopProvider.MaxCompletionTokens
		}
		ownedBy := model.OwnedBy
		if ownedBy == "" {
			ownedBy = strings.SplitN(model.ID, "/", 2)[0]
		}
		metadata := ModelMetadata{
			ID:            model.ID,
			Name:          model.Name,
			OwnedBy:       ownedBy,
			ContextWindow: contextWindow,
			MaxTokens:     maxTokens,
			Pricing:       model.Pricing,
		}
		for _, option := range model.ReasoningOptions {
			value := option.Type
			if len(option.Values) > 0 {
				value += ":" + strings.Join(option.Values, ",")
			}
			if option.Min > 0 || option.Max > 0 {
				value += fmt.Sprintf(":%d-%d", option.Min, option.Max)
			}
			metadata.ReasoningOptions = append(metadata.ReasoningOptions, value)
		}
		for _, parameter := range model.SupportedParameters {
			if parameter == "reasoning" || parameter == "include_reasoning" {
				metadata.ReasoningOptions = append(metadata.ReasoningOptions, "supported:"+parameter)
			}
		}
		cache.Models[model.ID] = metadata
	}
	if len(cache.Models) == 0 {
		return errors.New("catalog returned no models")
	}
	if err := c.writeCache(cache); err != nil {
		return err
	}
	c.cache = cache
	return nil
}

func (c *CatalogClient) readCache() (*modelCatalogCache, error) {
	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		return nil, err
	}
	var cache modelCatalogCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}
	if cache.Source != c.URL {
		return nil, fmt.Errorf("catalog cache source %q does not match %q", cache.Source, c.URL)
	}
	return &cache, nil
}

func (c *CatalogClient) writeCache(cache *modelCatalogCache) error {
	if err := os.MkdirAll(filepath.Dir(c.CachePath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.CachePath), ".model-catalog-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, c.CachePath)
}

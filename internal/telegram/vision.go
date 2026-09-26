package telegram

import (
	"context"

	"ceraa/harness/llm"
	"ceraa/harness/tool"
)

// providerVision adapts the active provider to what view_image needs.
//
// It resolves the model per call, the same way the compactor does, so switching
// provider or a refreshed credential takes effect immediately rather than being
// frozen into the tool at construction.
type providerVision struct {
	resolve func(ctx context.Context) (llm.ProviderConfig, error)
	// host is used to key learned media refusals per provider rather than per
	// model name, since the same name behind two providers can differ.
	host func(model string) string
}

func (p *providerVision) config(ctx context.Context) (llm.ProviderConfig, error) {
	if p.resolve == nil {
		return llm.ProviderConfig{}, errNoProvider
	}
	return p.resolve(ctx)
}

func (p *providerVision) ModelName() string {
	cfg, err := p.config(context.Background())
	if err != nil {
		return ""
	}
	return cfg.Model
}

func (p *providerVision) MediaHost() string {
	cfg, err := p.config(context.Background())
	if err != nil {
		return ""
	}
	if p.host != nil {
		return p.host(cfg.Model)
	}
	return cfg.BaseURL
}

func (p *providerVision) DescribeMedia(ctx context.Context, media []*llm.MediaAttachment, prompt string) (string, error) {
	cfg, err := p.config(ctx)
	if err != nil {
		return "", err
	}
	// A media call is its own short request: no session cache key, and a fresh
	// client so a credential refresh is picked up.
	cfg.PromptCacheMode = "off"
	client := llm.New(cfg)
	return client.DescribeMedia(ctx, media, prompt)
}

// errNoProvider is what view_image reports when nothing is configured, phrased
// as a configuration fact rather than a crash.
var errNoProvider = noProviderError{}

type noProviderError struct{}

func (noProviderError) Error() string {
	return "no provider is configured, so there is no model to look at the image with"
}

// attachVision gives every view_image in a registry a working client, and teaches
// the media-support cache which host a model belongs to.
func attachVision(registry *tool.Registry, resolve func(ctx context.Context) (llm.ProviderConfig, error), host func(model string) string) {
	vision := &providerVision{resolve: resolve, host: host}
	llm.SetMediaSupportHostResolver(func(model string) string {
		cfg, err := resolve(context.Background())
		if err != nil {
			return hostOrEmpty(host, model)
		}
		return hostOrEmpty(host, cfg.Model)
	})
	if existing, ok := registry.Get("view_image"); ok {
		if tool, ok := existing.(*tool.ViewImageTool); ok {
			tool.Client = vision
		}
	}
}

func hostOrEmpty(host func(string) string, model string) string {
	if host == nil {
		return ""
	}
	return host(model)
}

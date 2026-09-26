package agent

import (
	"context"

	"ceraa/harness/llm"
	"ceraa/harness/tool"
)

// attachVisionToRegistry gives view_image the run's resolved model outside
// Telegram too, so the CLI sees the same behaviour rather than a stub.
func attachVisionToRegistry(registry *tool.Registry, resolve func(ctx context.Context) (llm.ProviderConfig, error)) {
	vision := &cliVision{resolve: resolve}
	llm.SetMediaSupportHostResolver(func(string) string { return "" })
	if existing, ok := registry.Get("view_image"); ok {
		if tool, ok := existing.(*tool.ViewImageTool); ok {
			tool.Client = vision
		}
	}
}

type cliVision struct {
	resolve func(ctx context.Context) (llm.ProviderConfig, error)
}

func (c *cliVision) config(ctx context.Context) (llm.ProviderConfig, error) {
	if c.resolve == nil {
		return llm.ProviderConfig{}, errNoProviderConfigured
	}
	return c.resolve(ctx)
}

func (c *cliVision) ModelName() string {
	cfg, err := c.config(context.Background())
	if err != nil {
		return ""
	}
	return cfg.Model
}

func (c *cliVision) MediaHost() string { return "" }

func (c *cliVision) DescribeMedia(ctx context.Context, media []*llm.MediaAttachment, prompt string) (string, error) {
	cfg, err := c.config(ctx)
	if err != nil {
		return "", err
	}
	// A media call is its own short request and must not join the session cache.
	cfg.PromptCacheMode = "off"
	return llm.New(cfg).DescribeMedia(ctx, media, prompt)
}

var errNoProviderConfigured = noProviderConfiguredError{}

type noProviderConfiguredError struct{}

func (noProviderConfiguredError) Error() string {
	return "no provider is configured, so there is no model to look at the image with"
}

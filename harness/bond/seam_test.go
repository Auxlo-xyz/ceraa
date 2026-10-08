package bond

import (
	"context"
	"testing"

	"ceraa/harness/llm"
)

// stubAsk swaps the model seam and returns a restore func.
func stubAsk(t *testing.T, fn func(payload string) (string, error)) func() {
	t.Helper()
	original := ask
	ask = func(ctx context.Context, cfg llm.ProviderConfig, payload string) (string, error) {
		return fn(payload)
	}
	return func() { ask = original }
}

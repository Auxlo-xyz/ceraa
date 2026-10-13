package operation

import (
	"context"
	"errors"
	"fmt"
)

// StructuredBatchItem is one operation in an internal batch. Sequential items
// are placed on Queue; other items may run in parallel up to maxConcurrency.
// This is an execution seam for harness code, not an LLM-facing tool format.
type StructuredBatchItem struct {
	ID         string
	Spec       Spec
	Sequential bool
	Queue      string
}

// RunStructuredBatch executes a bounded batch through a dedicated manager and
// returns results in input order. A dedicated manager is deliberate: consuming
// a shared Results channel would be unsafe because it could steal another
// session's result. Existing operation dispatch, timeouts, redaction, lanes,
// and concurrency limits remain authoritative.
func RunStructuredBatch(ctx context.Context, runner ToolRunner, items []StructuredBatchItem, maxConcurrency int) ([]ResultEvent, error) {
	if runner == nil {
		return nil, errors.New("structured batch runner is nil")
	}
	if err := validateStructuredBatch(items); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return []ResultEvent{}, nil
	}
	if maxConcurrency <= 0 {
		maxConcurrency = len(items)
	}

	manager := NewWithBuffer(runner, len(items))
	manager.SetConcurrencyLimit(maxConcurrency)
	added := make(chan struct{})
	go func() {
		defer close(added)
		for _, item := range items {
			op := Operation{ID: item.ID, Spec: item.Spec, Status: StatusPending}
			if item.Sequential {
				manager.AddSequential(op, item.Queue)
			} else {
				manager.Add(op)
			}
		}
	}()

	byID := make(map[string]ResultEvent, len(items))
	for len(byID) < len(items) {
		select {
		case <-ctx.Done():
			manager.CancelAll()
			return nil, ctx.Err()
		case result := <-manager.Results():
			if _, exists := byID[result.OpID]; exists {
				return nil, fmt.Errorf("structured batch returned duplicate result %q", result.OpID)
			}
			byID[result.OpID] = result
		}
	}
	ordered := make([]ResultEvent, len(items))
	for i, item := range items {
		ordered[i] = byID[item.ID]
	}
	return ordered, nil
}

func validateStructuredBatch(items []StructuredBatchItem) error {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item.ID == "" {
			return errors.New("structured batch operation ID is empty")
		}
		if _, exists := seen[item.ID]; exists {
			return fmt.Errorf("structured batch operation ID %q is duplicated", item.ID)
		}
		seen[item.ID] = struct{}{}
		if item.Spec.Type == "" {
			return fmt.Errorf("structured batch operation %q has no type", item.ID)
		}
	}
	return nil
}

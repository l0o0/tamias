package desktop

import (
	"context"
	"errors"
)

type promptResult struct {
	value string
	err   error
}

// PromptWithContext lets the caller stop waiting for a native dialog when its
// owning application is shutting down. The native dialog API is not
// cancellable, so it runs in a goroutine and writes to a buffered result
// channel; a late result cannot block or be used by the canceled caller.
func PromptWithContext(ctx context.Context, prompt func() (string, error)) (string, error) {
	if ctx == nil {
		return "", errors.New("对话框上下文不能为空")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if prompt == nil {
		return "", errors.New("对话框不可用")
	}

	resultCh := make(chan promptResult, 1)
	go func() {
		if err := ctx.Err(); err != nil {
			resultCh <- promptResult{err: err}
			return
		}
		value, err := prompt()
		resultCh <- promptResult{value: value, err: err}
	}()

	select {
	case result := <-resultCh:
		// If a completion and shutdown race, do not let a selected path start a
		// write after the owning application has begun exiting.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return result.value, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

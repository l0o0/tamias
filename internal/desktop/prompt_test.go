package desktop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestPromptWithContextReturnsOnShutdownAndDiscardsLateSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	callbackDone := make(chan struct{})
	var applied atomic.Int32

	result := make(chan error, 1)
	go func() {
		path, err := PromptWithContext(ctx, func() (string, error) {
			close(started)
			<-release
			close(callbackDone)
			return "/tmp/late-selection", nil
		})
		if err == nil && path != "" {
			applied.Add(1)
		}
		result <- err
	}()
	<-started
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("prompt result error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt wrapper waited for the native dialog after shutdown")
	}

	close(release)
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("fake native dialog did not return")
	}
	if got := applied.Load(); got != 0 {
		t.Fatalf("late dialog selection was applied %d times after cancellation", got)
	}
}

func TestPromptWithContextSkipsPromptWhenAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	value, err := PromptWithContext(ctx, func() (string, error) {
		calls.Add(1)
		return "unexpected", nil
	})
	if value != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled prompt returned %q, %v", value, err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("pre-canceled context invoked native dialog %d times", got)
	}
}

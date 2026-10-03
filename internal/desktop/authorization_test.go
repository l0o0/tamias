package desktop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthorizationRequestCoalescesPendingCallersAndBoundsWait(t *testing.T) {
	const timeout = 250 * time.Millisecond
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var calls atomic.Int32
	var saved atomic.Int32

	check := NewAuthorizationRequest(func() (bool, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			close(finished)
			return true, nil
		}
		return false, nil
	}, timeout)

	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	results := make(chan error, 3)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			allowed, err := check()
			if err == nil && allowed {
				saved.Add(1)
			}
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	<-started

	for range 2 {
		select {
		case err := <-results:
			if err == nil || !strings.Contains(err.Error(), "系统通知授权未完成") || !strings.Contains(err.Error(), "当前设置未保存") {
				t.Fatalf("timed-out caller error = %v", err)
			}
		case <-time.After(2 * timeout):
			t.Fatal("authorization caller did not return within its bound")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent callers issued %d OS requests, want 1", got)
	}

	// A retry while the OS still owns the pending request must wait for that
	// same request, then time out independently instead of issuing another one.
	if _, err := check(); err == nil || !strings.Contains(err.Error(), "当前设置未保存") {
		t.Fatalf("repeat request while pending returned %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("retry issued a second OS request while one was pending: %d", got)
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("late OS request did not finish")
	}
	if got := saved.Load(); got != 0 {
		t.Fatalf("late OS result changed preferences after callers timed out: %d saves", got)
	}

	// The fake callback closes finished just before returning. The goroutine
	// coordinating the flight still has to publish its result and clear pending,
	// so a caller in that small interval correctly joins the old request. Keep
	// retrying until a call starts after that completion has been published.
	freshDeadline := time.After(2 * time.Second)
	for {
		select {
		case <-freshDeadline:
			t.Fatalf("fresh request was not started after the completed flight; calls=%d", calls.Load())
		default:
		}
		allowed, err := check()
		if calls.Load() == 1 {
			continue
		}
		if err != nil || allowed {
			t.Fatalf("fresh request returned allowed=%v err=%v, want denied without error", allowed, err)
		}
		break
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("fresh completed request count=%d, want 2", got)
	}
}

func TestAuthorizationRequestReturnsNativeResultsAndCanRetryAfterError(t *testing.T) {
	nativeErr := errors.New("native authorization failed")
	var calls atomic.Int32
	check := NewAuthorizationRequest(func() (bool, error) {
		switch calls.Add(1) {
		case 1:
			return false, nativeErr
		case 2:
			return false, nil
		default:
			return true, nil
		}
	}, time.Second)

	if allowed, err := check(); allowed || !errors.Is(err, nativeErr) {
		t.Fatalf("first result allowed=%v err=%v, want original native error", allowed, err)
	}
	if allowed, err := check(); allowed || err != nil {
		t.Fatalf("denial result allowed=%v err=%v, want false and nil", allowed, err)
	}
	if allowed, err := check(); !allowed || err != nil {
		t.Fatalf("approval result allowed=%v err=%v, want true and nil", allowed, err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("completed requests=%d, want 3 fresh native queries", got)
	}
}

func TestAuthorizationRequestStopsWaitingWhenApplicationShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var saved atomic.Int32
	check := NewAuthorizationRequestWithContext(ctx, func() (bool, error) {
		close(started)
		<-release
		close(finished)
		return true, nil
	}, time.Minute)

	result := make(chan error, 1)
	go func() {
		allowed, err := check()
		if err == nil && allowed {
			saved.Add(1)
		}
		result <- err
	}()
	<-started
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("authorization result error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("authorization caller waited for the OS callback after shutdown")
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("fake OS authorization callback did not return")
	}
	if got := saved.Load(); got != 0 {
		t.Fatalf("late OS result changed preferences %d times after cancellation", got)
	}
}

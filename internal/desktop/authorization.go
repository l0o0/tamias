package desktop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type authorizationResult struct {
	allowed bool
	err     error
}

type authorizationFlight struct {
	waiters map[chan authorizationResult]struct{}
}

// NewAuthorizationRequest bounds how long callers wait for a native OS
// notification authorization request. Concurrent callers share one in-flight
// request; a timed-out caller does not cancel the OS request, and its eventual
// result is delivered only to callers still waiting for it.
//
// Once the native request completes, a later call starts a fresh request so
// the OS can report permission changes made in system settings.
func NewAuthorizationRequest(request func() (bool, error), timeout time.Duration) func() (bool, error) {
	return NewAuthorizationRequestWithContext(context.Background(), request, timeout)
}

// NewAuthorizationRequestWithContext additionally stops waiting when the
// application context is canceled. The native OS request itself is not
// cancellable; its late result is discarded when no callers remain.
func NewAuthorizationRequestWithContext(ctx context.Context, request func() (bool, error), timeout time.Duration) func() (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var mu sync.Mutex
	var pending *authorizationFlight

	return func() (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		mu.Lock()
		flight := pending
		start := flight == nil
		if start {
			flight = &authorizationFlight{waiters: make(map[chan authorizationResult]struct{})}
			pending = flight
		}
		resultCh := make(chan authorizationResult, 1)
		flight.waiters[resultCh] = struct{}{}
		mu.Unlock()

		if start {
			go func() {
				result := authorizationResult{}
				if err := ctx.Err(); err != nil {
					result.err = err
				} else {
					result = callAuthorizationRequest(request)
				}
				mu.Lock()
				for waiter := range flight.waiters {
					// Each waiter owns a buffered channel. Do not let a caller that
					// timed out hold up completion of the native request.
					select {
					case waiter <- result:
					default:
					}
				}
				flight.waiters = nil
				if pending == flight {
					pending = nil
				}
				mu.Unlock()
			}()
		}

		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case result := <-resultCh:
			if err := ctx.Err(); err != nil {
				return false, err
			}
			return result.allowed, result.err
		case <-ctx.Done():
			mu.Lock()
			delete(flight.waiters, resultCh)
			mu.Unlock()
			return false, ctx.Err()
		case <-timer.C:
			if err := ctx.Err(); err != nil {
				mu.Lock()
				delete(flight.waiters, resultCh)
				mu.Unlock()
				return false, err
			}
			mu.Lock()
			delete(flight.waiters, resultCh)
			mu.Unlock()
			return false, errors.New("系统通知授权未完成，请检查系统设置中的通知权限，完成后再试；当前设置未保存")
		}
	}
}

func callAuthorizationRequest(request func() (bool, error)) (result authorizationResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = authorizationResult{err: fmt.Errorf("系统通知授权请求异常：%v", recovered)}
		}
	}()
	if request == nil {
		return authorizationResult{err: errors.New("系统通知授权请求不可用")}
	}
	result.allowed, result.err = request()
	return result
}

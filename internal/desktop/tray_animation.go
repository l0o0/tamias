package desktop

import (
	"context"
	"sync/atomic"
	"time"
)

// NewTrayAnimation returns a non-blocking click animation. Repeated clicks
// share the current playback, and app shutdown stops it without restoring an
// icon on a tray that may already have been destroyed. setIcon must marshal
// updates to the UI thread, check ctx there before touching the native tray,
// and return only after the update is applied or ctx is canceled.
func NewTrayAnimation(ctx context.Context, frames [][]byte, still []byte, interval time.Duration, setIcon func([]byte)) func() {
	var playing atomic.Bool
	return func() {
		if ctx.Err() != nil || !playing.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer playing.Store(false)
			timer := time.NewTimer(interval)
			defer timer.Stop()
			for _, frame := range frames {
				if ctx.Err() != nil {
					return
				}
				setIcon(frame)
				// Space out frame requests while allowing shutdown to interrupt
				// the delay immediately.
				timer.Reset(interval)
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
			}
			if ctx.Err() == nil {
				setIcon(still)
			}
		}()
	}
}

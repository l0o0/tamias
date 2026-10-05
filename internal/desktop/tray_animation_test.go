package desktop

import (
	"context"
	"testing"
	"time"
)

func awaitTrayFrame(t *testing.T, updates <-chan string, want string) {
	t.Helper()
	select {
	case got := <-updates:
		if got != want {
			t.Fatalf("tray frame = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for tray frame %q", want)
	}
}

func TestTrayAnimationRestoresIconAndCoalescesClicks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan string, 10)
	firstFrame := make(chan struct{})
	release := make(chan struct{})
	restoring := make(chan struct{})
	restoreRelease := make(chan struct{})
	play := NewTrayAnimation(ctx, [][]byte{[]byte("left"), []byte("right")}, []byte("still"), time.Millisecond, func(icon []byte) {
		if string(icon) == "left" {
			close(firstFrame)
			<-release
		}
		if string(icon) == "still" {
			close(restoring)
			<-restoreRelease
		}
		updates <- string(icon)
	})
	play()
	select {
	case <-firstFrame:
	case <-time.After(time.Second):
		t.Fatal("animation did not start")
	}
	for range 20 {
		play()
	}
	close(release)
	awaitTrayFrame(t, updates, "left")
	awaitTrayFrame(t, updates, "right")
	select {
	case <-restoring:
	case <-time.After(time.Second):
		t.Fatal("animation did not restore the static icon")
	}
	// Even if the UI is slow to restore the static icon, a click must not
	// begin another playback until that restoration has completed.
	for range 20 {
		play()
	}
	close(restoreRelease)
	awaitTrayFrame(t, updates, "still")
	select {
	case extra := <-updates:
		t.Fatalf("overlapping playback produced %q", extra)
	case <-time.After(10 * time.Millisecond):
	}
}

func TestTrayAnimationStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan string, 10)
	play := NewTrayAnimation(ctx, [][]byte{[]byte("left"), []byte("right")}, []byte("still"), 20*time.Millisecond, func(icon []byte) {
		updates <- string(icon)
	})
	play()
	awaitTrayFrame(t, updates, "left")
	cancel()
	play()
	select {
	case extra := <-updates:
		t.Fatalf("shutdown produced late tray update %q", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

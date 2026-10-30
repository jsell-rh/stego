package process

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestSupervisorBoundaries(t *testing.T) {
	check := func(t *testing.T, modes []string) {
		for _, mode := range modes {
			t.Run(mode, func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				release := make(chan struct{})
				joined := make(chan struct{})
				work := func(ctx context.Context, ready, stopping chan struct{}) int {
					defer close(joined)
					switch mode {
					case "return":
						return 0
					case "panic":
						panic("private-process panic")
					case "goexit":
						runtime.Goexit()
					case "startup-timeout":
						<-release
						return 0
					case "cleanup-timeout":
						close(ready)
						close(stopping)
						<-release
						return 0
					case "cancel":
						close(ready)
						cancel()
						<-ctx.Done()
						close(stopping)
						return 0
					}
					return 1
				}
				started := time.Now()
				got := superviseWork(parent, 30*time.Millisecond, 30*time.Millisecond, work)
				expected := 1
				if mode == "return" || mode == "cancel" {
					expected = 0
				}
				if got != expected || time.Since(started) > time.Second {
					t.Fatal("supervisor failed", mode, got)
				}
				close(release)
				{
					select {
					case <-joined:
					case <-time.After(time.Second):
						t.Fatal("work did not join")
					}
				}
			})
		}
	}
	t.Run("ST-SUP-RC08-OC03-T01 The emitted process supervisor bounds startup and cleanup", func(t *testing.T) {
		check(t, []string{"startup-timeout", "cleanup-timeout", "cancel"})
	})
	t.Run("ST-SUP-RC08-OC04-T01 The emitted process supervisor survives a failing callback", func(t *testing.T) {
		check(t, []string{"panic", "goexit"})
	})
	check(t, []string{"return"})
}

package keycloak

import (
	"context"
	"errors"
	runtime "example.com/provider/out/controller"
	"fmt"
	"testing"
	"time"
)

func waitLifecyclePending(t *testing.T, c *Client, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.lifecycleGates.mu.Lock()
		n := c.lifecycleGates.pending
		c.lifecycleGates.mu.Unlock()
		if n == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("lifecycle request did not enter the gate")
}
func TestNativeLifecycleGateAcrossJournalInstances(t *testing.T) {
	c, _, _, _ := newNativeAccessFixture(t)
	_, f := newLifecycleFixture(t, false)
	firstEntered, secondEntered, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	journal := func(entered chan struct{}, hold bool) *runtime.StateJournal {
		t.Helper()
		j, err := runtime.NewStateJournal(f.protector, f.key, runtime.StatePersistence{
			Load: func(ctx context.Context) (runtime.SealedStateRecord, error) {
				close(entered)
				if hold {
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				return runtime.SealedStateRecord{}, runtime.ErrStateJournal
			},
			Save: func(context.Context, int64, []byte) (runtime.SealedStateRecord, error) {
				return runtime.SealedStateRecord{}, runtime.ErrStateJournal
			},
		}, 60<<10)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	native, err := NewNativeClientLifecycle(c, journal(firstEntered, true), f.provider.identity)
	if err != nil {
		t.Fatal(err)
	}
	account, err := NewServiceAccountClientLifecycle(c, journal(secondEntered, false), f.provider.identity)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := native.Reconcile(ctx, lifecyclePolicy); first <- err }()
	select {
	case <-firstEntered:
	case <-ctx.Done():
		t.Fatal("first lifecycle did not load")
	}
	go func() { second <- account.Close(ctx) }()
	waitLifecyclePending(t, c, 2)
	select {
	case <-secondEntered:
		t.Fatal("same-client lifecycle overlapped")
	default:
	}
	close(release)
	if !errors.Is(<-first, runtime.ErrStateJournal) || !errors.Is(<-second, runtime.ErrStateJournal) {
		t.Fatal("journal failure was lost")
	}
	select {
	case <-secondEntered:
	default:
		t.Fatal("waiting lifecycle did not proceed")
	}
	waitLifecyclePending(t, c, 0)
}
func TestNativeLifecycleGateCapacityCancellationAndClose(t *testing.T) {
	c, _, _, _ := newNativeAccessFixture(t)
	work, release, err := c.beginClientLifecycle(context.Background(), "same")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() {
		_, done, err := c.beginClientLifecycle(ctx, "same")
		if done != nil {
			done()
		}
		waited <- err
	}()
	waitLifecyclePending(t, c, 2)
	_, other, err := c.beginClientLifecycle(context.Background(), "other")
	if err != nil {
		t.Fatal("unrelated client was blocked", err)
	}
	other()
	cancel()
	if !errors.Is(<-waited, context.Canceled) {
		t.Fatal("queued cancellation was lost")
	}
	var releases []func()
	for i := 1; i < MaxPendingClientLifecycles; i++ {
		_, done, err := c.beginClientLifecycle(context.Background(), fmt.Sprint("key-", i))
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, done)
	}
	if _, _, err := c.beginClientLifecycle(context.Background(), "overflow"); !errors.Is(err, ErrCapacity) {
		t.Fatal("unbounded lifecycle admission")
	}
	for _, done := range releases {
		done()
		done()
	}
	waitLifecyclePending(t, c, 1)
	c.Close()
	select {
	case <-work.Done():
	case <-time.After(time.Second):
		t.Fatal("provider close did not cancel lifecycle")
	}
	release()
	c.lifecycleGates.mu.Lock()
	left := len(c.lifecycleGates.keys)
	c.lifecycleGates.mu.Unlock()
	if left != 0 {
		t.Fatal("released client keys remain cached")
	}
	if _, _, err := c.beginClientLifecycle(context.Background(), "closed"); !errors.Is(err, ErrClosed) {
		t.Fatal("closed provider admitted lifecycle")
	}
}

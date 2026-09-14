package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func scope(ns string) CollectionScope {
	return CollectionScope{Collection: Collection{Path: "/api/v1/namespaces/" + ns + "/pods"}, Identity: "uid-" + ns}
}
func scopedPod(ns, uid string) Object {
	return Object{"metadata": Object{"namespace": ns, "uid": uid, "resourceVersion": "1"}, "status": Object{"phase": "Running"}}
}
func takeSet[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("watch set did not make progress")
		var zero T
		return zero
	}
}
func startSet(t *testing.T, source CollectionObserver, options WatchSetOptions) (*WatchSet, context.Context, chan ScopedChange, chan error) {
	t.Helper()
	set, err := NewWatchSet(source, options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan ScopedChange, 128)
	done := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() {
		done <- set.Run(ctx, func(change ScopedChange) error {
			select {
			case events <- change:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	t.Cleanup(func() { cancel(); workers.Wait() })
	return set, ctx, events, done
}
func awaitBaseline(t *testing.T, events <-chan ScopedChange, names ...string) {
	t.Helper()
	pending := map[string]bool{}
	for _, name := range names {
		pending[scope(name).Collection.Path] = true
	}
	for len(pending) > 0 {
		event := takeSet(t, events)
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if event.Change.Type == "REPLACE" {
			delete(pending, event.Scope.Collection.Path)
		}
	}
}

func TestWatchSetAddsRemovesAndReplacesNamespaces(t *testing.T) {
	opened, closed := make(chan string, 16), make(chan string, 16)
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != 6 || parts[3] != "namespaces" {
			t.Error("unscoped request", r.URL.Path)
			w.WriteHeader(403)
			return
		}
		ns := parts[4]
		if r.URL.Query().Get("watch") == "true" {
			_ = json.NewEncoder(w).Encode(Object{"type": "BOOKMARK", "object": Object{"metadata": Object{"resourceVersion": "1"}}})
			w.(http.Flusher).Flush()
			opened <- ns
			<-r.Context().Done()
			closed <- ns
			return
		}
		_ = json.NewEncoder(w).Encode(Object{"metadata": Object{"resourceVersion": "1"}, "items": []Object{scopedPod(ns, "pod-"+ns)}})
	})
	set, ctx, events, _ := startSet(t, client, WatchSetOptions{})
	if err := set.Replace(ctx, []CollectionScope{scope("one"), scope("two")}); err != nil {
		t.Fatal(err)
	}
	awaitBaseline(t, events, "one", "two")
	takeSet(t, opened)
	takeSet(t, opened)
	if err := set.Replace(ctx, []CollectionScope{scope("two"), scope("three")}); err != nil {
		t.Fatal(err)
	}
	if got := takeSet(t, closed); got != "one" {
		t.Fatal("wrong watch closed", got)
	}
	event := takeSet(t, events)
	if event.Change.Type != "REMOVED" || event.Scope != scope("one") {
		t.Fatal("missing scope removal", event)
	}
	awaitBaseline(t, events, "three")
	if got := takeSet(t, opened); got != "three" {
		t.Fatal("unchanged watch restarted", got)
	}
	replacement := scope("two")
	replacement.Identity = "new-namespace-uid"
	if err := set.Replace(ctx, []CollectionScope{replacement, scope("three")}); err != nil {
		t.Fatal(err)
	}
	if got := takeSet(t, closed); got != "two" {
		t.Fatal("old namespace identity stayed active", got)
	}
	event = takeSet(t, events)
	if event.Change.Type != "REMOVED" || event.Scope.Identity != scope("two").Identity {
		t.Fatal("old identity was not removed")
	}
	for {
		event = takeSet(t, events)
		if event.Change.Type == "REPLACE" {
			if event.Scope != replacement {
				t.Fatal("replacement has old identity")
			}
			break
		}
	}
	if got := takeSet(t, opened); got != "two" {
		t.Fatal("replacement watch did not start", got)
	}
	if err := set.Replace(ctx, nil); err != nil {
		t.Fatal(err)
	}
	takeSet(t, closed)
	takeSet(t, closed)
}

func TestWatchSetRejectsInvalidAssignmentWithoutStoppingCurrentWatch(t *testing.T) {
	opened, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			opened <- struct{}{}
			<-r.Context().Done()
			closed <- struct{}{}
			return
		}
		_ = json.NewEncoder(w).Encode(Object{"metadata": Object{"resourceVersion": "1"}, "items": []Object{}})
	})
	set, ctx, events, _ := startSet(t, client, WatchSetOptions{MaxCollections: 1})
	if err := set.Replace(ctx, []CollectionScope{scope("one")}); err != nil {
		t.Fatal(err)
	}
	awaitBaseline(t, events, "one")
	takeSet(t, opened)
	invalid := []CollectionScope{}
	for _, path := range []string{"/api/v1/pods", "/apis/group/v1/pods", "/api/v1/namespaces/one/pods/name", "/api/v1/namespaces/../pods", "/api/v1/namespaces/one/pods?watch=true", "/api/v1/namespaces/one/pods/status", "/api/v1/namespaces/%6fne/pods", "/api/v1/namespaces//pods"} {
		bad := scope("one")
		bad.Collection.Path = path
		invalid = append(invalid, bad)
	}
	bad := scope("one")
	bad.Identity = ""
	invalid = append(invalid, bad)
	for _, bad := range invalid {
		if err := set.Replace(ctx, []CollectionScope{bad}); !errors.Is(err, ErrWatchSetContract) {
			t.Fatal("invalid scope accepted", bad, err)
		}
	}
	if err := set.Replace(ctx, []CollectionScope{scope("one"), scope("two")}); !errors.Is(err, ErrWatchSetContract) {
		t.Fatal("scope limit was exceeded", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := set.Replace(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled replacement accepted", err)
	}
	select {
	case <-closed:
		t.Fatal("invalid assignment closed an existing watch")
	default:
	}
	if err := set.Replace(ctx, nil); err != nil {
		t.Fatal(err)
	}
	takeSet(t, closed)
}

func TestWatchSetRetriesDeniedScopeOnlyAfterFreshAssignment(t *testing.T) {
	var allowed atomic.Bool
	var calls atomic.Int32
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !allowed.Load() {
			w.WriteHeader(403)
			return
		}
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(Object{"metadata": Object{"resourceVersion": "1"}, "items": []Object{}})
	})
	set, ctx, events, _ := startSet(t, client, WatchSetOptions{})
	if err := set.Replace(ctx, []CollectionScope{scope("one")}); err != nil {
		t.Fatal(err)
	}
	for {
		event := takeSet(t, events)
		if event.Change.Type == "REPLACE" {
			t.Fatal("denied list produced an empty baseline")
		}
		if event.Err != nil {
			if !errors.Is(event.Err, ErrWatchAccess) {
				t.Fatal(event.Err)
			}
			break
		}
	}
	if calls.Load() != 1 {
		t.Fatal("denied scope retried without a fresh assignment")
	}
	allowed.Store(true)
	if err := set.Replace(ctx, []CollectionScope{scope("one")}); err != nil {
		t.Fatal(err)
	}
	awaitBaseline(t, events, "one")
}

func TestWatchSetRejectsAggregateLimitsAndForeignObjects(t *testing.T) {
	for _, kind := range []string{"objects", "bytes", "namespace"} {
		t.Run(kind, func(t *testing.T) {
			client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				ns := strings.Split(r.URL.Path, "/")[4]
				if r.URL.Query().Get("watch") == "true" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				if kind == "namespace" {
					ns = "foreign"
				}
				_ = json.NewEncoder(w).Encode(Object{"metadata": Object{"resourceVersion": "1"}, "items": []Object{scopedPod(ns, "pod-"+ns)}})
			})
			options := WatchSetOptions{}
			if kind == "objects" {
				options.MaxObjects = 1
			}
			if kind == "bytes" {
				encoded, _ := json.Marshal(scopedPod("one", "pod-one"))
				options.MaxBytes = len(encoded) + 1
			}
			set, ctx, events, done := startSet(t, client, options)
			if err := set.Replace(ctx, []CollectionScope{scope("one"), scope("two")}); err != nil {
				t.Fatal(err)
			}
			if err := takeSet(t, done); !errors.Is(err, ErrWatchSetContract) {
				t.Fatal("invalid collection set survived", err)
			}
			replacements := 0
			for len(events) > 0 {
				event := <-events
				if event.Change.Type == "REPLACE" {
					replacements++
				}
			}
			if replacements > 1 || kind == "namespace" && replacements != 0 {
				t.Fatal("invalid baseline reached the consumer")
			}
		})
	}
}

type setSource struct {
	run func(context.Context, Collection, func(Change) error) error
}

func (s setSource) WatchLimit() int { return 16 }
func (s setSource) Observe(ctx context.Context, c Collection, emit func(Change) error) error {
	return s.run(ctx, c, emit)
}

func TestWatchSetSerializesBaselinesAndJoinsRemovedWatch(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	stopped := make(chan string, 2)
	var first sync.Once
	source := setSource{run: func(ctx context.Context, c Collection, emit func(Change) error) error {
		defer func() { stopped <- c.Path }()
		if err := emit(Change{Type: "RESET"}); err != nil {
			return err
		}
		entered <- c.Path
		wait := false
		first.Do(func() { wait = true })
		if wait {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := emit(Change{Type: "REPLACE"}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	set, ctx, events, _ := startSet(t, source, WatchSetOptions{})
	if err := set.Replace(ctx, []CollectionScope{scope("one"), scope("two")}); err != nil {
		t.Fatal(err)
	}
	takeSet(t, entered)
	select {
	case <-entered:
		t.Fatal("two complete baselines ran at once")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	takeSet(t, entered)
	awaitBaseline(t, events, "one", "two")
	if err := set.Replace(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 2 {
		t.Fatal("replacement returned before old watches stopped")
	}
}

func TestWatchSetPropagatesConsumerFailureAndCanRestart(t *testing.T) {
	source := setSource{run: func(ctx context.Context, _ Collection, emit func(Change) error) error {
		if err := emit(Change{Type: "RESET"}); err != nil {
			return err
		}
		if err := emit(Change{Type: "REPLACE"}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	set, err := NewWatchSet(source, WatchSetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		done := make(chan error, 1)
		want := errors.New("consumer stopped")
		go func() { done <- set.Run(ctx, func(ScopedChange) error { return want }) }()
		if err := set.Replace(ctx, []CollectionScope{scope("one")}); err != nil {
			cancel()
			t.Fatal(err)
		}
		if err := takeSet(t, done); !errors.Is(err, want) {
			cancel()
			t.Fatal("consumer error changed", err)
		}
		cancel()
	}
}

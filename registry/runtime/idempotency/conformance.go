package idempotency

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Call is one request that Conform sends through a store's wrapped write.
type Call struct {
	Key  string
	Body []byte
	// Inside, when not nil, is what the store's handler calls inside the write transaction, after its write and
	// before it answers. An error from Inside fails the call.
	Inside func(ctx context.Context) error
	// Crash asks the store to end its own database session after Inside and before the commit, as a crash of the
	// process does. Do then returns the error of the lost session.
	Crash bool
}

// Subject is a store that binds the wrapper, as its own test code gives it to Conform. Each field is a value of the
// service's code; Conform calls nothing else of the service.
type Subject struct {
	// Do sends one request with the call's key and body through the store's wrapped write, and returns the response
	// that the service sends.
	Do func(ctx context.Context, call Call) (Response, error)
	// KeyRows counts the committed key rows of key in the store's key table, in the scope that Do uses.
	KeyRows func(ctx context.Context, key string) (int, error)
	// Body and OtherBody are two valid request bodies with different fingerprints.
	Body, OtherBody []byte
	// Wait is the longest time that Conform waits for a held request; zero is 10 s.
	Wait time.Duration
}

// Failure is one case of Conform that the subject failed, with the reason.
type Failure struct {
	Case   string
	Reason string
}

func (f Failure) String() string { return f.Case + ": " + f.Reason }

// The cases of Conform (ST-SUP-RP07-LC08).
const (
	CaseReplay = "replay"
	CaseReuse  = "reuse"
	CaseRace   = "race"
	CaseCrash  = "crash"
)

// Conform runs the conformance suite of the wrapper against s and returns the cases that s fails, or nothing when s
// passes (ST-SUP-RP07-LC08). The suite proves that a repeat of the same body replays the stored response with
// Idempotent-Replayed: true and runs the handler once, that a different body with a used key gets 422, that a
// duplicate sent while the first request is still in its handler gets 409 with no handler run and no wait for the
// first, and that a crash before the commit leaves no key row. Each run uses new random keys. Every store that
// binds Wrap calls Conform from its tests.
func Conform(ctx context.Context, s Subject) []Failure {
	wait := s.Wait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	prefix := randomPrefix()
	var failures []Failure
	for _, c := range []struct {
		name string
		run  func(context.Context, Subject, string, time.Duration) string
	}{
		{CaseReplay, conformReplay},
		{CaseReuse, conformReuse},
		{CaseRace, conformRace},
		{CaseCrash, conformCrash},
	} {
		if reason := c.run(ctx, s, prefix+"-"+c.name, wait); reason != "" {
			failures = append(failures, Failure{Case: c.name, Reason: reason})
		}
	}
	return failures
}

func conformReplay(ctx context.Context, s Subject, key string, _ time.Duration) string {
	var runs atomic.Int32
	count := func(context.Context) error { runs.Add(1); return nil }
	first, err := s.Do(ctx, Call{Key: key, Body: s.Body, Inside: count})
	if err != nil {
		return fmt.Sprintf("the first request failed: %v", err)
	}
	if !stored(first.Status) {
		return fmt.Sprintf("the first request got %d, not a response that is stored", first.Status)
	}
	again, err := s.Do(ctx, Call{Key: key, Body: s.Body, Inside: count})
	if err != nil {
		return fmt.Sprintf("the repeat failed: %v", err)
	}
	switch {
	case runs.Load() != 1:
		return fmt.Sprintf("the handler ran %d times, not once", runs.Load())
	case again.Status != first.Status:
		return fmt.Sprintf("the replay got %d, not the stored %d", again.Status, first.Status)
	case !bytes.Equal(again.Body, first.Body):
		return "the replayed body is not the stored body byte for byte"
	case again.Header.Get(ReplayedHeader) != "true":
		return "the replay has no Idempotent-Replayed: true"
	case !sameHeader(first.Header, again.Header):
		return "the replayed headers are not the stored headers"
	}
	return ""
}

func conformReuse(ctx context.Context, s Subject, key string, _ time.Duration) string {
	if _, err := s.Do(ctx, Call{Key: key, Body: s.Body}); err != nil {
		return fmt.Sprintf("the first request failed: %v", err)
	}
	var runs atomic.Int32
	got, err := s.Do(ctx, Call{Key: key, Body: s.OtherBody, Inside: func(context.Context) error {
		runs.Add(1)
		return nil
	}})
	switch {
	case err != nil:
		return fmt.Sprintf("422 expected, the request failed: %v", err)
	case got.Status != http.StatusUnprocessableEntity:
		return fmt.Sprintf("422 expected, got %d", got.Status)
	case runs.Load() != 0:
		return "422 expected without a handler run, and the handler ran"
	}
	return ""
}

func conformRace(ctx context.Context, s Subject, key string, wait time.Duration) string {
	ctx, cancel := context.WithTimeout(ctx, 3*wait)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	type outcome struct {
		resp Response
		err  error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		resp, err := s.Do(ctx, Call{Key: key, Body: s.Body, Inside: func(ctx context.Context) error {
			if once.CompareAndSwap(false, true) {
				close(entered)
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
		firstDone <- outcome{resp, err}
	}()
	defer func() { <-firstDone }()
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	select {
	case <-entered:
	case r := <-firstDone:
		firstDone <- r
		return fmt.Sprintf("the first request ended before its handler ran: %v", r.err)
	case <-time.After(wait):
		return "the first request did not reach its handler"
	}

	var runs atomic.Int32
	secondDone := make(chan outcome, 1)
	go func() {
		resp, err := s.Do(ctx, Call{Key: key, Body: s.Body, Inside: func(context.Context) error {
			runs.Add(1)
			return nil
		}})
		secondDone <- outcome{resp, err}
	}()
	var second outcome
	select {
	case second = <-secondDone:
	case <-time.After(wait):
		releaseOnce()
		second = <-secondDone
		return fmt.Sprintf("409 expected at once, the duplicate waited for the first request and got %d", second.resp.Status)
	}
	releaseOnce()
	switch {
	case second.err != nil:
		return fmt.Sprintf("409 expected, the duplicate failed: %v", second.err)
	case second.resp.Status != http.StatusConflict:
		return fmt.Sprintf("409 expected, got %d", second.resp.Status)
	case runs.Load() != 0:
		return "409 expected without a handler run, and the duplicate's handler ran"
	}
	first := <-firstDone
	firstDone <- first
	if first.err != nil {
		return fmt.Sprintf("the first request failed after the duplicate: %v", first.err)
	}
	return ""
}

func conformCrash(ctx context.Context, s Subject, key string, _ time.Duration) string {
	// The error of the lost session is expected; what counts is the key table after it.
	_, _ = s.Do(ctx, Call{Key: key, Body: s.Body, Crash: true})
	n, err := s.KeyRows(ctx, key)
	if err != nil {
		return fmt.Sprintf("the key rows could not be counted: %v", err)
	}
	if n != 0 {
		return fmt.Sprintf("a crash before the commit left %d key rows", n)
	}
	return ""
}

// stored reports a status that Wrap stores and that the suite expects of a valid first request.
func stored(status int) bool {
	return status >= 200 && status < 500 && status != http.StatusConflict && status != http.StatusTooManyRequests &&
		status != http.StatusUnprocessableEntity
}

// sameHeader compares two headers, apart from Idempotent-Replayed.
func sameHeader(a, b http.Header) bool {
	names := func(h http.Header) []string {
		var out []string
		for name := range h {
			if !strings.EqualFold(name, ReplayedHeader) {
				out = append(out, http.CanonicalHeaderKey(name))
			}
		}
		slices.Sort(out)
		return out
	}
	na, nb := names(a), names(b)
	if !slices.Equal(na, nb) {
		return false
	}
	for _, name := range na {
		if !slices.Equal(a.Values(name), b.Values(name)) {
			return false
		}
	}
	return true
}

func randomPrefix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "conform-" + hex.EncodeToString(b[:])
}

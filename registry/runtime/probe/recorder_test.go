package probe

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeT is a test double for testing.T that keeps the failures.
type fakeT struct{ failures []string }

func (f *fakeT) Helper() {}
func (f *fakeT) Errorf(format string, args ...any) {
	f.failures = append(f.failures, fmt.Sprintf(format, args...))
}

func TestProbeRecorder(t *testing.T) {
	t.Run("ST-SUP-RP12-LC10-T01 The recorder keeps every emission in order from concurrent callers", func(t *testing.T) {
		clearEnvironment(t)
		const goroutines, each = 16, 625
		var workers []string
		for g := range goroutines {
			workers = append(workers, "g"+strconv.Itoa(g))
		}
		seq := declare(t, Spec{Name: "sequenced", Kind: Event, Severity: Info, EmittedBy: []string{command}, Fields: []Field{
			{Name: "worker", Type: FieldEnum, Values: workers}, {Name: "seq", Type: FieldInteger},
		}})
		recorder := NewRecorder(nil)
		// A runtime with every other sink exists beside the recorder, and gets nothing.
		stdout := &lines{}
		other := newProbe(t, newTelemetry(t), "json", stdout)
		var group sync.WaitGroup
		for g := range goroutines {
			group.Go(func() {
				for i := 0; i < each; {
					call := recorder.Bind(context.Background(), "t1", "user:u-7", command)
					for j := 0; j < 25 && i < each; j, i = j+1, i+1 {
						call.Probe().Emit(seq, Enum(workers[g]), Integer(int64(i)))
					}
					call.Commit()
				}
			})
		}
		group.Wait()
		other.Close(context.Background())
		all := recorder.All()
		if len(all) != goroutines*each || recorder.Dropped() != 0 {
			t.Fatalf("the recorder holds %d emissions with %d drops, want %d and none", len(all), recorder.Dropped(), goroutines*each)
		}
		next := map[string]int{}
		for _, e := range all {
			worker, n := e.Fields[0].Value, e.Fields[1].Value
			if n != strconv.Itoa(next[worker]) {
				t.Fatalf("the emission %s of %s comes when %d is next", n, worker, next[worker])
			}
			next[worker]++
		}
		if len(stdout.all()) != 0 || other.Delivery() != (Delivery{}) {
			t.Fatal("another sink got a record of the recorder's probe")
		}
	})

	t.Run("ST-SUP-RP12-LC10-T02 A fill test ends the call with a rollback and sees the rolled_back attempt", func(t *testing.T) {
		refused := reservationRefused(t, CountsAttempt)
		made := reservationsMade(t)
		for _, commit := range []bool{false, true} {
			recorder := NewRecorder(nil)
			// The generated test constructor binds the probe with a fixed tenant, principal and emitter name.
			call := recorder.Bind(context.Background(), "tenant-test", "principal-test", command)
			call.Probe().Emit(refused, Text("A-1"), Enum("short"))
			call.Probe().Emit(made)
			call.Returned()
			if commit {
				call.Commit()
			} else {
				call.Rollback()
			}
			got := recorder.Emissions("reservation_refused")
			want := outcomeRolledBack
			if commit {
				want = outcomeCommitted
			}
			if len(got) != 1 || got[0].Outcome != want {
				t.Fatalf("with a commit %v reservation_refused is %v, want one with stego.outcome=%s", commit, got, want)
			}
			if n := len(recorder.Emissions("reservations_made")); (n == 1) != commit {
				t.Fatalf("with a commit %v the recorder holds %d reservations_made", commit, n)
			}
		}
	})

	t.Run("ST-SUP-RP12-LC11-T01 A fill test passes or fails on the recorded emission", func(t *testing.T) {
		refused := reservationRefused(t, CountsAttempt)
		recorder := NewRecorder(nil)
		call := recorder.Bind(context.Background(), "tenant-test", "principal-test", command)
		call.Probe().Emit(refused, Text("A-1"), Enum("short"))
		call.Commit()

		pass := &fakeT{}
		recorder.ExpectEmitted(pass, "reservation_refused", FieldValue{Name: "reason", Value: "short"})
		if len(pass.failures) != 0 {
			t.Fatalf("the check for reason=short failed: %v", pass.failures)
		}
		for name, check := range map[string]func(TB){
			"reason=expired": func(t TB) {
				recorder.ExpectEmitted(t, "reservation_refused", FieldValue{Name: "reason", Value: "expired"})
			},
			"not emitted": func(t TB) { recorder.ExpectNotEmitted(t, "reservation_refused") },
		} {
			fail := &fakeT{}
			check(fail)
			if len(fail.failures) != 1 {
				t.Fatalf("the check %s gave %d failures, want 1", name, len(fail.failures))
			}
			if msg := fail.failures[0]; !strings.Contains(msg, "reservation_refused") || !strings.Contains(msg, "reservation_refused{sku=A-1 reason=short stego.outcome=committed}") {
				t.Fatalf("the failure of %s is %q, want the observation and the recorded emission", name, msg)
			}
		}
	})
}

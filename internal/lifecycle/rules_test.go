package lifecycle

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

// request is a supersession of digest of component name by successor, released on 2026-03-01, with no
// declaration or fill change, at stage beta.
func request(t *testing.T, name, from string, successor Subject) SupersedeRequest {
	return SupersedeRequest{
		Superseded:             Subject{Kind: ComponentPackage, Name: name, Digest: from},
		SupersededStage:        "beta",
		Successor:              successor,
		SuccessorStage:         "beta",
		SuccessorReleasedOn:    date(t, "2026-03-01"),
		NeedsDeclarationChange: no(), NeedsFillChange: no(),
		Summary:    "the successor fixes the request log",
		RecordedOn: time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC),
	}
}

func component(name, d string) Subject {
	return Subject{Kind: ComponentPackage, Name: name, Digest: d}
}

func TestOneSuccessorOfTheSameComponentOrOfTheGraduatedOne(t *testing.T) {
	t.Run("ST-DIS-INV06-T A digest takes one successor, of its own component or of the graduated component that took over its port with a declaration change", func(t *testing.T) {
		d1, d2, d3 := digest("1"), digest("2"), digest("3")
		var a Lifecycle
		if _, err := a.supersede(request(t, "a", d1, component("a", d2)), "STEGO-2026-0001"); err != nil {
			t.Fatalf("the first supersession of D1 by D2 is refused: %v", err)
		}
		if a.Supersession == nil || a.Supersession.Successor.Digest != d2 {
			t.Fatalf("the first supersession is not recorded: %+v", a)
		}
		refused := func(l *Lifecycle, r SupersedeRequest, wants ...string) {
			t.Helper()
			before := *l
			_, err := l.supersede(r, "STEGO-2026-0009")
			if err == nil {
				t.Fatalf("the supersession of %s by %s is recorded; want a refusal", r.Superseded, r.Successor)
			}
			for _, want := range wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
			}
			if !reflect.DeepEqual(*l, before) {
				t.Errorf("the refusal changed the lifecycle: %+v, was %+v", *l, before)
			}
		}
		refused(&a, request(t, "a", d1, component("a", d3)), "already has the successor", d2)

		var self Lifecycle
		refused(&self, request(t, "a", d1, component("a", d1)), "another digest")

		var byC Lifecycle
		r := request(t, "a", d1, component("c", digest("c")))
		r.SupersededProvides = []string{"http-server"}
		r.SuccessorStage = "ga"
		r.NeedsDeclarationChange = yes()
		refused(&byC, r, "other component c", "took over no port")

		o1 := digest("4")
		port := func(successor Subject, stage string, declaration *bool) SupersedeRequest {
			r := request(t, "o", o1, successor)
			r.SupersededProvides = []string{"port-p"}
			r.SuccessorTookOver = []string{"port-p"}
			r.SuccessorStage = stage
			r.NeedsDeclarationChange = declaration
			return r
		}
		var o Lifecycle
		refused(&o, port(component("b", digest("b")), "beta", yes()), "did not graduate to ga")
		refused(&o, port(component("g", digest("5")), "ga", no()), "needs a declaration change")
		if _, err := o.supersede(port(component("g", digest("5")), "ga", yes()), "STEGO-2026-0002"); err != nil {
			t.Fatalf("the supersession of O1 by G1 with a declaration change is refused: %v", err)
		}
		if o.Supersession == nil || o.Supersession.Successor != component("g", digest("5")) || !o.Supersession.NeedsDeclarationChange {
			t.Fatalf("the supersession of O1 by G1 is not recorded: %+v", o.Supersession)
		}
	})
}

func TestFailDateFollowsTheStageWindow(t *testing.T) {
	t.Run("ST-DIS-INV12-T A supersession's fail date is the successor's release date plus its stage window, or earlier", func(t *testing.T) {
		recorded := func(stage string, declaration *bool, failsAfter string) (Lifecycle, error) {
			r := request(t, "a", digest("1"), component("a", digest("2")))
			r.SupersededStage = stage
			r.NeedsDeclarationChange = declaration
			if failsAfter != "" {
				day := date(t, failsAfter)
				r.FailsAfter = &day
			}
			var l Lifecycle
			_, err := l.supersede(r, "STEGO-2026-0001")
			return l, err
		}
		for _, c := range []struct {
			name, stage string
			declaration *bool
			want        string
		}{
			{"alpha with no declaration change", "alpha", no(), "2026-03-01"},
			{"beta with no declaration change", "beta", no(), "2026-06-01"},
			{"ga with no declaration change", "ga", no(), "2026-09-01"},
			{"beta with a declaration change", "beta", yes(), "2026-09-01"},
		} {
			l, err := recorded(c.stage, c.declaration, "")
			if err != nil {
				t.Fatalf("%s: refused: %v", c.name, err)
			}
			if got := FormatDate(l.Supersession.FailsAfter); got != c.want {
				t.Errorf("%s: the fail date is %s, want %s", c.name, got, c.want)
			}
		}
		l, err := recorded("beta", no(), "2026-05-01")
		if err != nil {
			t.Fatalf("the shortened date is refused: %v", err)
		}
		if got := FormatDate(l.Supersession.FailsAfter); got != "2026-05-01" {
			t.Errorf("the shortened fail date is %s, want 2026-05-01", got)
		}
		l, err = recorded("beta", no(), "2026-06-02")
		if err == nil {
			t.Fatalf("the date one day after the window is recorded: %+v", l.Supersession)
		}
		if !strings.Contains(err.Error(), "2026-06-01") {
			t.Errorf("the refusal %q does not name the latest date allowed, 2026-06-01", err)
		}
		if l.Supersession != nil {
			t.Errorf("the refusal recorded a supersession: %+v", l.Supersession)
		}
	})
}

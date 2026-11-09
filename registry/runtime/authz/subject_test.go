package authz

import (
	"context"
	"strings"
	"testing"

	"stego.dev/runtime/internal/seal"
)

// identity is a verified identity of the tests. The test is in the runtime module, so it can embed the seal's mark.
type identity struct {
	seal.IdentityMark
	userID string
}

func (i identity) TenantClaim() (string, bool) { return "t1", true }
func (i identity) UserID() string              { return i.userID }

func TestSubjects(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP06-LC07-T01 An identity maps to the declared subject type and ID", func(t *testing.T) {
		subject, err := SubjectOf("user", identity{userID: "u-42"})
		if err != nil {
			t.Fatal(err)
		}
		if subject.String() != "user:u-42" {
			t.Fatalf("the subject is %s, want user:u-42", subject)
		}
		if _, err := SubjectOf("user", identity{userID: "*"}); err == nil {
			t.Fatal("an identity maps to the wildcard subject")
		}
	})

	t.Run("ST-SUP-RP06-LC07-T02 A worker, a controller and the relay call with their own service account subject", func(t *testing.T) {
		fake := &recorder{}
		rt := newUnitRuntime(t, fake, nil)
		relay, err := NewRelay(RelayConfig{Name: "authz-relay", Relationships: &fakeRelationships{}, DB: fakeDB(t),
			Schema: "public", Permanent: func(err error) error { return err }})
		if err != nil {
			t.Fatal(err)
		}
		worker, err := System("sweeper")
		if err != nil {
			t.Fatal(err)
		}
		controller, err := System("reconciler")
		if err != nil {
			t.Fatal(err)
		}
		for _, principal := range []Principal{worker, controller, relay.Principal()} {
			handle := bindUnit(t, rt, principal)
			if _, err := handle.Evaluate(ctx, "view", doc("1"), Token{}, nil); err != nil {
				t.Fatal(err)
			}
		}
		var got []string
		for _, request := range fake.evaluations {
			if request.Subject.IsZero() {
				t.Error("a request has no subject")
			}
			got = append(got, request.Subject.String())
		}
		want := []string{"service_account:sweeper", "service_account:reconciler", "service_account:authz-relay"}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("the subjects are %v, want %v", got, want)
		}
		if _, err := rt.Bind(fakeTx(t), mustTenant(t, "t1"), Principal{}, nil); err == nil {
			t.Fatal("a handle is bound with no principal")
		}
	})

	t.Run("ST-SUP-RP06-LC07-T03 The package has no bypass flag, setting or subject", func(t *testing.T) {
		_, files := packageSource(t)
		identifiers, literals := identifiersAndStrings(t, files)
		for _, word := range append(append([]string(nil), identifiers...), literals...) {
			folded := strings.ToLower(word)
			for _, bad := range []string{"bypass", "skip", "allow_all", "allowall", "superuser", "insecure"} {
				if strings.Contains(folded, bad) {
					t.Errorf("the package holds %q, which names %s", word, bad)
				}
			}
		}
		for _, literal := range literals {
			if literal == "*" || strings.HasSuffix(literal, ":*") {
				t.Errorf("the package holds the wildcard subject %q", literal)
			}
		}
		for _, name := range identifiers {
			if name == "Getenv" || name == "LookupEnv" || name == "Environ" {
				t.Errorf("the package reads the environment through %s", name)
			}
		}
		// Every evaluation that allows has one provider request behind it.
		fake := &recorder{allow: func(EvaluationRequest) bool { return true }}
		rt := newUnitRuntime(t, fake, nil)
		person, err := Person("user", identity{userID: "u-7"})
		if err != nil {
			t.Fatal(err)
		}
		handle := bindUnit(t, rt, person)
		allowed := 0
		check := func(ok bool) {
			t.Helper()
			if ok {
				allowed++
			}
			if n := fake.requests(); n != allowed {
				t.Fatalf("%d allows came from %d provider requests", allowed, n)
			}
		}
		result, err := rt.Port().Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
			Resource: doc("1"), Consistency: Fresh()})
		if err != nil {
			t.Fatal(err)
		}
		check(result.Allowed)
		result, err = handle.Evaluate(ctx, "view", doc("1"), Token{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(result.Allowed)
		_, ok, err := handle.DecideTenant(ctx, "provision", Resource{Type: "platform", ID: "main"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(ok)
		rows, err := handle.List("view", nil).Page(ctx, []Row{{Resource: doc("1")}})
		if err != nil {
			t.Fatal(err)
		}
		check(len(rows) == 1)
		if _, err := rt.Port().Evaluate(ctx, EvaluationRequest{Subject: Subject{Type: "user", ID: "*"},
			Action: Action{Name: "view"}, Resource: doc("1"), Consistency: Fresh()}); err == nil {
			t.Fatal("the wildcard subject is evaluated")
		}
		check(false)
	})

	t.Run("ST-SUP-RP06-LC08-T01 The caveat context holds only the declared context fields", func(t *testing.T) {
		context := NewContext([]string{"region", "amount"}, map[string]any{"region": "eu", "amount": 12.5, "note": "x"})
		if len(context) != 2 || context["region"] != "eu" || context["amount"] != 12.5 {
			t.Fatalf("the context is %v, want exactly region and amount", context)
		}
		if _, ok := context["note"]; ok {
			t.Fatal("the context holds the undeclared field note")
		}
	})
}

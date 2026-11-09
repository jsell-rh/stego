package authz

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stego.dev/runtime/authn"
	"stego.dev/runtime/authn/authntest"
	"stego.dev/runtime/internal/seal"
	"stego.dev/runtime/tenant"
)

// verifiedIdentity gives a genuine identity of the authentication runtime for the user ID userID: it mints a token
// with authntest and verifies it with authn.
func verifiedIdentity(t *testing.T, userID string) authn.Identity {
	t.Helper()
	const issuer, audience = "https://issuer.authz.test/realms/r1", "authz-api"
	key := authntest.NewKey(t, "k1")
	set, err := authn.ParseKeys(authntest.Document(key.JWK()))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := authn.New(authn.Settings{Issuer: issuer, Audience: audience, ProblemCode: "SVC", Keys: set,
		TenantClaim: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(verifier.Stop)
	claims := authntest.Claims(issuer, audience, userID, time.Now())
	claims["tenant"] = "t1"
	identity, err := verifier.Verify(context.Background(), authntest.Mint(t, authntest.Header("k1"), claims, key))
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestSubjects(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP06-LC07-T01 An identity maps to the declared subject type and ID", func(t *testing.T) {
		subject, err := SubjectOf("user", verifiedIdentity(t, "u-42"))
		if err != nil {
			t.Fatal(err)
		}
		if subject.String() != "user:u-42" {
			t.Fatalf("the subject is %s, want user:u-42", subject)
		}
		if _, err := SubjectOf("user", verifiedIdentity(t, "*")); err == nil {
			t.Fatal("an identity maps to the wildcard subject")
		}
		if _, err := SubjectOf("user", authn.Identity{}); err == nil {
			t.Fatal("the zero identity maps to a subject")
		}
	})

	t.Run("ST-SUP-RP06-LC07-T02 A worker, a controller and the relay call with their own service account subject", func(t *testing.T) {
		fake := &recorder{}
		rt := newUnitRuntime(t, fake)
		relay, err := NewRelay(RelayConfig{Name: "authz-relay", Relationships: &fakeRelationships{}, DB: fakeDB(t),
			Schema: "public"})
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
			handle := bindUnit(t, rt, nil, principal)
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
		if _, err := rt.Bind(floorTx(t, &floorSeq{}), mustTenant(t, "t1"), Principal{}, nil); err == nil {
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
		rt := newUnitRuntime(t, fake)
		handle := bindUnit(t, rt, nil, mustPerson(t, "u-7"))
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
		_, ok, err := handle.DecideTenant(ctx, tenant.ProvisionAction, "t9", Resource{Type: "platform", ID: "main"}, nil)
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

	t.Run("ST-SUP-RP06-LC19-T01 Only RunConsumer can make the staging principal, and it maps to the staging subject", func(t *testing.T) {
		principal, err := StagingPrincipal(seal.NewStaging(), "user:alice")
		if err != nil || principal.Subject().String() != "user:alice" {
			t.Fatalf("the staging principal of user:alice is %v, %v", principal.Subject(), err)
		}
		for _, column := range []string{"", "user:", "*", "user:*", "user"} {
			if _, err := StagingPrincipal(seal.NewStaging(), column); err == nil {
				t.Errorf("the column %q gave a principal", column)
			}
		}
		if _, err := StagingPrincipal(seal.Staging{}, "user:alice"); err == nil {
			t.Error("the zero staging key gave a principal")
		}
		// A package outside the module cannot import the internal seal package, so it cannot make the key.
		err = buildOutside(t, "package outside\n\nimport (\n\t\"stego.dev/runtime/authz\"\n\t\"stego.dev/runtime/internal/seal\"\n)\n\n"+
			"func Use() { _, _ = authz.StagingPrincipal(seal.NewStaging(), \"user:alice\") }\n")
		if err == nil || !strings.Contains(err.Error(), "internal package") {
			t.Fatalf("the outside package gave %v, want the build to refuse the internal package", err)
		}
	})
}

// buildOutside builds a package of a module outside the runtime module, which resolves stego.dev/runtime from this
// checkout through the work file of scripts/runtime-workspace.sh, and gives the build's error with its output.
func buildOutside(t *testing.T, src string) error {
	t.Helper()
	dir := t.TempDir()
	module := filepath.Join(dir, "outside")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example.com/outside\n\ngo 1.26.10\n\nrequire stego.dev/runtime v0.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "outside.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../../scripts/runtime-workspace.sh")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", script, module, filepath.Join(dir, "outside.work")).Output()
	if err != nil {
		t.Fatalf("write the work file: %v", err)
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = module
	build.Env = append(os.Environ(), "GOWORK="+strings.TrimSpace(string(out)))
	output, err := build.CombinedOutput()
	if err != nil {
		return errors.New(string(output))
	}
	return nil
}

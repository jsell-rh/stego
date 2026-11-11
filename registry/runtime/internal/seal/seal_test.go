package seal_test

import (
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
)

// embedsPointer is an outer type that embeds a nil *authz.Principal: the promoted method of the mark panics on it.
type embedsPointer struct{ *authz.Principal }

// embedsValue is an outer type that embeds a genuine principal and declares a Subject method of its own.
type embedsValue struct{ authz.Principal }

func (embedsValue) Subject() authz.Subject { return authz.Subject{Type: "user", ID: "forged"} }

// embedsInterface is an outer type that embeds a nil seal.Principal.
type embedsInterface struct{ seal.Principal }

func TestPrincipalSeal(t *testing.T) {
	t.Run("ST-SUP-RP01-LC17-T04 Only a principal of the authorization runtime gives a subject through the seal", func(t *testing.T) {
		ops, err := authz.System("ops")
		if err != nil {
			t.Fatal(err)
		}
		if subject, ok := seal.SubjectOf(ops); !ok || subject != "service_account:ops" {
			t.Fatalf("a genuine principal gave %q, %v", subject, ok)
		}
		// An outer type cannot change the subject: the seal reads the mark, never a method of the outer type.
		if subject, ok := seal.SubjectOf(embedsValue{ops}); !ok || subject != "service_account:ops" {
			t.Fatalf("an outer type with its own Subject gave %q, %v", subject, ok)
		}
		// Two principals of one subject stay equal.
		again, err := authz.System("ops")
		if err != nil || again != ops {
			t.Fatalf("two principals of one subject are not equal: %v", err)
		}
		var nilPointer *authz.Principal
		for name, p := range map[string]seal.Principal{
			"nil":                          nil,
			"the zero authz.Principal":     authz.Principal{},
			"a zero mark":                  seal.PrincipalMark{},
			"a nil *authz.Principal":       nilPointer,
			"an embedded nil pointer":      embedsPointer{},
			"an embedded nil interface":    embedsInterface{},
			"a mark with an empty subject": seal.NewPrincipalMark(""),
		} {
			if subject, ok := seal.SubjectOf(p); ok || subject != "" {
				t.Errorf("%s gave the subject %q", name, subject)
			}
		}
	})
}

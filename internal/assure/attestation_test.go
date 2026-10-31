package assure

import (
	"strings"
	"testing"
)

func TestAttestation(t *testing.T) {
	signer := Signer{Issuer: "https://token.actions.githubusercontent.com", Subject: "https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml@refs/heads/main"}
	commit := "0123456789abcdef0123456789abcdef01234567"

	t.Run("ST-VO56-C01-T An attestation names its subject by a full commit, never a tag, a branch or a short ID", func(t *testing.T) {
		attestation, err := NewAttestation(ProofRunPredicateType, commit, signer, "")
		if err != nil {
			t.Fatalf("a full commit is refused: %v", err)
		}
		if attestation.Subject != commit {
			t.Fatalf("subject = %q, want %q", attestation.Subject, commit)
		}
		for name, subject := range map[string]string{
			"a tag":                       "v1.2.0",
			"a branch":                    "main",
			"a branch reference":          "refs/heads/main",
			"a short ID":                  commit[:7],
			"one digit short":             commit[:39],
			"one digit long":              commit + "8",
			"upper-case digits":           strings.ToUpper(commit),
			"a character that is not hex": "g123456789abcdef0123456789abcdef01234567",
			"no subject":                  "",
		} {
			if _, err := NewAttestation(ProofRunPredicateType, subject, signer, ""); err == nil {
				t.Fatalf("%s (%q) is accepted as the subject", name, subject)
			}
		}
	})
}

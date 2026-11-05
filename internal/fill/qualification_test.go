package fill

import (
	"strings"
	"testing"
	"time"
)

func TestIsQualified(t *testing.T) {
	t.Run("ST-EXT-SP01-T A fill is qualified only when its evidence names both current digests and a listed key signed that evidence", func(t *testing.T) {
		k := makeTestKeys(t)
		signers := mustSigners(t, k.signers)
		fillDigest, contractDigest := strings.Repeat("1", 64), strings.Repeat("2", 64)
		signed := func(change func(*Fill), key string) *Fill {
			f := unsignedFill()
			change(f)
			digest := mustEvidenceDigest(t, f)
			f.SignOff = &FillSignOff{EvidenceDigest: digest, Signature: Signature{KeyType: KeySSH, Signer: "listed@example.com", Armored: k.sshSign(t, key, digest)}, SignedAt: time.Now()}
			return f
		}
		same := func(*Fill) {}
		if f := signed(same, k.listedSSH); !IsQualified(f, fillDigest, contractDigest, signers) {
			t.Fatal("a fill with current evidence and a sign-off by listed-ssh is not qualified")
		}
		otherEvidence := signed(same, k.listedSSH)
		earlier := unsignedFill()
		earlier.Evidence.PassedAt = earlier.Evidence.PassedAt.Add(-time.Hour)
		earlierDigest := mustEvidenceDigest(t, earlier)
		otherEvidence.SignOff.EvidenceDigest = earlierDigest
		otherEvidence.SignOff.Signature.Armored = k.sshSign(t, k.listedSSH, earlierDigest)
		noSignOff := signed(same, k.listedSSH)
		noSignOff.SignOff = nil
		noEvidence := signed(same, k.listedSSH)
		noEvidence.Evidence = nil
		for name, f := range map[string]*Fill{
			"an earlier fill digest":       signed(func(f *Fill) { f.Evidence.FillDigest = strings.Repeat("9", 64) }, k.listedSSH),
			"an earlier contract digest":   signed(func(f *Fill) { f.Evidence.ContractDigest = strings.Repeat("9", 64) }, k.listedSSH),
			"no evidence":                  noEvidence,
			"evidence and no sign-off":     noSignOff,
			"a sign-off by unlisted-ssh":   signed(same, k.unlistedSSH),
			"a sign-off of other evidence": otherEvidence,
		} {
			if IsQualified(f, fillDigest, contractDigest, signers) {
				t.Errorf("a fill with %s is qualified", name)
			}
		}
	})
}

func TestStandingValues(t *testing.T) {
	t.Run("ST-VO45-C01-T A qualification standing is exactly one of qualified, stale, unsigned or untested", func(t *testing.T) {
		for _, text := range []string{"qualified", "stale", "unsigned", "untested"} {
			if standing, err := ParseStanding(text); err != nil || string(standing) != text {
				t.Errorf("%q: refused %v", text, err)
			}
		}
		for _, text := range []string{"Qualified", "qualified ", "signed", "expired", ""} {
			if standing, err := ParseStanding(text); err == nil {
				t.Errorf("%q: accepted as %q", text, standing)
			}
		}
	})
}

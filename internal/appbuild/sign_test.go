package appbuild

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var trustedTestSigner = SignerIdentity{Issuer: "https://token.actions.githubusercontent.com", Subject: "https://github.com/example/platform/.github/workflows/image.yml@refs/heads/main"}

// signFixture writes the image folder that BuildImage writes: the OCI layout,
// image.json, build.json and both SBOMs. It gives the folder, the image record
// and the trusted digest of image.json.
func signFixture(t *testing.T) (string, *ImageRecord, string, []SBOM) {
	t.Helper()
	root := t.TempDir()
	ca := imageTestCA(t, true)
	application := []byte("fixed application bytes")
	put(t, root, "application", string(application))
	put(t, root, "trust.pem", string(ca))
	build := sbomFixtureRecord()
	build.Artifact = Artifact{digest(application), int64(len(application))}
	buildData, err := json.MarshalIndent(build, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	buildData = append(buildData, '\n')
	put(t, root, "build.json", string(buildData))
	record := &ImageRecord{Format: 1, PackerCompiler: build.BuildCompiler, PackerCompilerArtifact: build.BuildCompilerArtifact, BuildRecordSHA256: digest(buildData), Application: build.Artifact, TrustStore: Artifact{digest(ca), int64(len(ca))}, Entrypoint: "service"}
	if err := assembleImage(context.Background(), root, record); err != nil {
		t.Fatal(err)
	}
	sboms, err := writeSBOMFiles(root, &build)
	if err != nil {
		t.Fatal(err)
	}
	recordData, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	recordData = append(recordData, '\n')
	put(t, root, "image.json", string(recordData))
	return root, record, digest(recordData), sboms
}

// fakeVerifier stands for the provider's signature check: it gives, for each
// bundle path, the attestation that a verified bundle holds, or an error.
type fakeVerifier map[string]VerifiedAttestation

func (verifier fakeVerifier) VerifyAttestation(ctx context.Context, bundle, subject, predicateType string, signer SignerIdentity) (VerifiedAttestation, error) {
	attestation, ok := verifier[bundle]
	if !ok {
		return VerifiedAttestation{}, errors.New("the signature does not verify")
	}
	return attestation, nil
}

func signedSBOM(record *ImageRecord, sbom SBOM) VerifiedAttestation {
	return VerifiedAttestation{Subjects: []string{"sha256:" + record.Manifest.SHA256}, PredicateType: sbom.Format.PredicateType(), Predicate: json.RawMessage(sbom.Document), Signer: trustedTestSigner, LogIndex: 4242, SignedAt: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
}

func signFixtureOptions(t *testing.T, root string, recordSHA256 string, verifier fakeVerifier) SignOptions {
	t.Helper()
	bundles := t.TempDir()
	put(t, bundles, "spdx.json", `{"bundle":"spdx"}`)
	put(t, bundles, "cdx.json", `{"bundle":"cdx"}`)
	return SignOptions{Image: root, RecordSHA256: recordSHA256, Check: SignatureCheck{SPDXBundle: filepath.Join(bundles, "spdx.json"), CycloneDXBundle: filepath.Join(bundles, "cdx.json"), Signer: trustedTestSigner, Verifier: verifier}}
}

func TestImageSignatureIsAcceptableOnlyFromTheTrustedSigner(t *testing.T) {
	t.Run("ST-BLD-SP02-T An image signature is acceptable only in the log or by a trusted key, and by the trusted signer", func(t *testing.T) {
		image := strings.Repeat("1", 64)
		valid := VerifiedAttestation{Subjects: []string{"sha256:" + image}, PredicateType: SPDX.PredicateType(), Signer: trustedTestSigner, LogIndex: 7, SignedAt: time.Unix(1, 0)}
		if err := AcceptableSignature(valid, trustedTestSigner, image); err != nil {
			t.Fatal(err)
		}
		key := SignerIdentity{PublicKeySHA256: strings.Repeat("2", 64)}
		keySigned := valid
		keySigned.Signer, keySigned.LogIndex = key, -1
		if err := AcceptableSignature(keySigned, key, image); err != nil {
			t.Fatal("a signature by the trusted key was refused", err)
		}
		refused := map[string]func(*VerifiedAttestation, *SignerIdentity){
			"not in the log": func(a *VerifiedAttestation, _ *SignerIdentity) { a.LogIndex = -1 },
			"another image": func(a *VerifiedAttestation, _ *SignerIdentity) {
				a.Subjects = []string{"sha256:" + strings.Repeat("3", 64)}
			},
			"two subjects": func(a *VerifiedAttestation, _ *SignerIdentity) { a.Subjects = append(a.Subjects, "sha256:"+image) },
			"another workflow": func(a *VerifiedAttestation, _ *SignerIdentity) {
				a.Signer.Subject = strings.Replace(a.Signer.Subject, "image.yml", "other.yml", 1)
			},
			"another reference": func(a *VerifiedAttestation, _ *SignerIdentity) {
				a.Signer.Subject = strings.Replace(a.Signer.Subject, "main", "candidate", 1)
			},
			"another issuer": func(a *VerifiedAttestation, _ *SignerIdentity) { a.Signer.Issuer = "https://issuer.example" },
			"another key": func(a *VerifiedAttestation, s *SignerIdentity) {
				*s = key
				a.Signer = SignerIdentity{PublicKeySHA256: strings.Repeat("4", 64)}
			},
			"no signing time":      func(a *VerifiedAttestation, _ *SignerIdentity) { a.SignedAt = time.Time{} },
			"untrusted signer set": func(_ *VerifiedAttestation, s *SignerIdentity) { *s = SignerIdentity{} },
		}
		for name, change := range refused {
			t.Run(name, func(t *testing.T) {
				attestation, trusted := valid, trustedTestSigner
				attestation.Subjects = append([]string{}, valid.Subjects...)
				change(&attestation, &trusted)
				if err := AcceptableSignature(attestation, trusted, image); err == nil {
					t.Fatal("an unacceptable signature was accepted")
				}
			})
		}
	})
}

func TestOnlyTheTrustedSignerIdentityIsAccepted(t *testing.T) {
	t.Run("ST-BLD-INV10-T A signature by any identity other than the pipeline's trusted signer is refused", func(t *testing.T) {
		if err := signedByTrustedSigner(trustedTestSigner, trustedTestSigner); err != nil {
			t.Fatal(err)
		}
		for name, signer := range map[string]SignerIdentity{
			"other workflow":  {Issuer: trustedTestSigner.Issuer, Subject: "https://github.com/example/fork/.github/workflows/image.yml@refs/heads/main"},
			"other reference": {Issuer: trustedTestSigner.Issuer, Subject: strings.TrimSuffix(trustedTestSigner.Subject, "main") + "feature"},
			"other issuer":    {Issuer: "https://accounts.example", Subject: trustedTestSigner.Subject},
			"key and keyless": {Issuer: trustedTestSigner.Issuer, Subject: trustedTestSigner.Subject, PublicKeySHA256: strings.Repeat("5", 64)},
			"no identity":     {},
			"a key, not it":   {PublicKeySHA256: strings.Repeat("6", 64)},
			"subject, no ref": {Issuer: trustedTestSigner.Issuer, Subject: "https://github.com/example/platform/.github/workflows/image.yml"},
		} {
			t.Run(name, func(t *testing.T) {
				if err := signedByTrustedSigner(signer, trustedTestSigner); err == nil {
					t.Fatal("another identity was accepted")
				}
			})
		}
		for name, trusted := range map[string]SignerIdentity{
			"both":           {Issuer: trustedTestSigner.Issuer, Subject: trustedTestSigner.Subject, PublicKeySHA256: strings.Repeat("5", 64)},
			"neither":        {},
			"no exact ref":   {Issuer: trustedTestSigner.Issuer, Subject: "https://github.com/example/platform/.github/workflows/image.yml@main"},
			"plain issuer":   {Issuer: "http://token.actions.githubusercontent.com", Subject: trustedTestSigner.Subject},
			"short key hash": {PublicKeySHA256: "abc"},
		} {
			t.Run("trusted "+name, func(t *testing.T) {
				if err := signedByTrustedSigner(trusted, trusted); err == nil {
					t.Fatal("an invalid trusted signer was accepted")
				}
			})
		}
		root, record, recordSHA256, sboms := signFixture(t)
		options := signFixtureOptions(t, root, recordSHA256, nil)
		other := trustedTestSigner
		other.Subject = strings.Replace(other.Subject, "platform", "fork", 1)
		verifier := fakeVerifier{}
		for index, bundle := range []string{options.Check.SPDXBundle, options.Check.CycloneDXBundle} {
			attestation := signedSBOM(record, sboms[index])
			attestation.Signer = other
			verifier[bundle] = attestation
		}
		options.Check.Verifier = verifier
		if _, err := SignImage(context.Background(), options); err == nil || !strings.Contains(err.Error(), "not by the trusted signer") {
			t.Fatal("an image signed by another identity was accepted", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "signature.json")); !os.IsNotExist(err) {
			t.Fatal("a refused signature was recorded", err)
		}
	})
}

func TestSignedImageCarriesBothSBOMs(t *testing.T) {
	t.Run("ST-BLD-INV09-T An image signature holds both SBOMs of its build record as signed attestations", func(t *testing.T) {
		for _, change := range []string{"", "missing CycloneDX", "SPDX twice", "changed predicate", "changed SBOM file", "unsigned bundle", "removed SBOM file"} {
			t.Run("case "+change, func(t *testing.T) {
				root, record, recordSHA256, sboms := signFixture(t)
				options := signFixtureOptions(t, root, recordSHA256, nil)
				verifier := fakeVerifier{options.Check.SPDXBundle: signedSBOM(record, sboms[0]), options.Check.CycloneDXBundle: signedSBOM(record, sboms[1])}
				switch change {
				case "missing CycloneDX":
					attestation := verifier[options.Check.CycloneDXBundle]
					attestation.PredicateType = "https://slsa.dev/provenance/v1"
					verifier[options.Check.CycloneDXBundle] = attestation
				case "SPDX twice":
					verifier[options.Check.CycloneDXBundle] = verifier[options.Check.SPDXBundle]
				case "changed predicate":
					attestation := verifier[options.Check.SPDXBundle]
					attestation.Predicate = json.RawMessage(strings.Replace(string(sboms[0].Document), "v0.41.0", "v0.40.0", 1))
					verifier[options.Check.SPDXBundle] = attestation
				case "changed SBOM file":
					put(t, root, CycloneDX.FileName(), "{}\n")
				case "unsigned bundle":
					delete(verifier, options.Check.CycloneDXBundle)
				case "removed SBOM file":
					if err := os.Remove(filepath.Join(root, SPDX.FileName())); err != nil {
						t.Fatal(err)
					}
				}
				options.Check.Verifier = verifier
				signature, err := SignImage(context.Background(), options)
				if change == "" {
					if err != nil {
						t.Fatal(err)
					}
					if len(signature.Attestations) != 2 || signature.Attestations[0].Format != SPDX || signature.Attestations[1].Format != CycloneDX || signature.Attestations[0].PredicateSHA256 != sboms[0].Digest || signature.Attestations[1].PredicateSHA256 != sboms[1].Digest {
						t.Fatal("the signature does not hold both SBOM attestations", signature.Attestations)
					}
					if signature.ImageSHA256 != record.Manifest.SHA256 || signature.TransparencyLogEntry != "rekor:4242" || signature.SignedAt != "2026-11-04T12:00:00Z" || signature.Signer != trustedTestSigner {
						t.Fatal("the signature record differs", signature)
					}
					if _, err := os.Stat(filepath.Join(root, "signature.json")); err != nil {
						t.Fatal("the signature was not recorded", err)
					}
					if _, err := SignImage(context.Background(), options); err == nil {
						t.Fatal("a recorded signature was replaced")
					}
					return
				}
				if err == nil {
					t.Fatal("an image without both signed SBOMs was accepted")
				}
				if _, err := os.Lstat(filepath.Join(root, "signature.json")); !os.IsNotExist(err) {
					t.Fatal("a refused signature was recorded", err)
				}
			})
		}
	})
}

func TestGitHubVerificationResultIsRead(t *testing.T) {
	t.Run("ST-BLD-SP02-T The provider's verified attestation names its subject, signer, log entry and time", func(t *testing.T) {
		output := `[{"attestation":{"bundle":{"verificationMaterial":{"tlogEntries":[{"logIndex":"123456"}]}}},
"verificationResult":{"statement":{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"image","digest":{"sha256":"` + strings.Repeat("1", 64) + `"}}],"predicateType":"https://spdx.dev/Document/v2.3","predicate":{"spdxVersion":"SPDX-2.3"}},
"signature":{"certificate":{"issuer":"https://token.actions.githubusercontent.com","subjectAlternativeName":"https://github.com/example/platform/.github/workflows/image.yml@refs/heads/main"}},
"verifiedTimestamps":[{"type":"Tlog","uri":"https://rekor.sigstore.dev","timestamp":"2026-11-04T12:00:00Z"}]}}]`
		attestation, err := readGitHubVerification([]byte(output))
		if err != nil {
			t.Fatal(err)
		}
		if err := AcceptableSignature(attestation, trustedTestSigner, strings.Repeat("1", 64)); err != nil {
			t.Fatal(err)
		}
		if attestation.LogIndex != 123456 || attestation.PredicateType != SPDX.PredicateType() || string(attestation.Predicate) != `{"spdxVersion":"SPDX-2.3"}` {
			t.Fatal("the verified attestation differs", attestation)
		}
		for name, text := range map[string]string{
			"no result":    `[]`,
			"two results":  `[{},{}]`,
			"not in-toto":  strings.Replace(output, "in-toto.io/Statement/v1", "example.test/statement", 1),
			"negative log": strings.Replace(output, `"123456"`, `"-1"`, 1),
			"not JSON":     `verified`,
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := readGitHubVerification([]byte(text)); err == nil {
					t.Fatal("an invalid verification result was read")
				}
			})
		}
		if _, err := (GitHubAttestationVerifier{GH: "gh", Repository: "example/platform"}).VerifyAttestation(context.Background(), "bundle", "subject", SPDX.PredicateType(), trustedTestSigner); err == nil {
			t.Fatal("a GitHub CLI that is not named by its absolute path was run")
		}
	})
}

func TestGitHubVerifierEnvironment(t *testing.T) {
	t.Run("ST-BLD-SP02-T The GitHub CLI gets the provider token and no other inherited variable", func(t *testing.T) {
		fixed := []string{"PATH=/usr/bin:/bin", "HOME=" + os.TempDir(), "GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1"}
		inherited := []string{"PATH=/opt/other/bin", "HOME=/home/operator", "GH_HOST=example.test", "GH_CONFIG_DIR=/home/operator/.gh",
			"HTTPS_PROXY=http://proxy.example.test", "SSL_CERT_FILE=/tmp/ca.pem", "LD_PRELOAD=/tmp/hook.so", "SECRET=value"}
		for name, test := range map[string]struct{ environ, want []string }{
			"no token":      {inherited, fixed},
			"GH_TOKEN":      {append([]string{"GH_TOKEN=first"}, inherited...), append(append([]string{}, fixed...), "GH_TOKEN=first")},
			"both tokens":   {append(append([]string{}, inherited...), "GITHUB_TOKEN=second", "GH_TOKEN=first"), append(append([]string{}, fixed...), "GITHUB_TOKEN=second", "GH_TOKEN=first")},
			"empty token":   {append([]string{"GH_TOKEN="}, inherited...), fixed},
			"token as part": {append([]string{"MY_GH_TOKEN=other", "GH_TOKEN_FILE=/tmp/token"}, inherited...), fixed},
		} {
			t.Run(name, func(t *testing.T) {
				got := verifierEnvironment(test.environ)
				if strings.Join(got, "\n") != strings.Join(test.want, "\n") {
					t.Fatalf("the GitHub CLI environment is %q, want %q", got, test.want)
				}
			})
		}
	})
}

func TestSignerIdentityIsKeylessOrKey(t *testing.T) {
	t.Run("ST-VO50-C01-T A signer identity is a keyless pipeline identity or a key, never both", func(t *testing.T) {
		for _, identity := range []SignerIdentity{trustedTestSigner, {PublicKeySHA256: strings.Repeat("7", 64)}} {
			if err := identity.Validate(); err != nil {
				t.Fatal(err)
			}
		}
		for name, identity := range map[string]SignerIdentity{
			"both":             {Issuer: trustedTestSigner.Issuer, Subject: trustedTestSigner.Subject, PublicKeySHA256: strings.Repeat("7", 64)},
			"neither":          {},
			"issuer only":      {Issuer: trustedTestSigner.Issuer},
			"subject only":     {Subject: trustedTestSigner.Subject},
			"no exact ref":     {Issuer: trustedTestSigner.Issuer, Subject: "https://github.com/example/platform/.github/workflows/image.yml"},
			"plain issuer":     {Issuer: "http://token.actions.githubusercontent.com", Subject: trustedTestSigner.Subject},
			"key not a digest": {PublicKeySHA256: "key"},
		} {
			t.Run(name, func(t *testing.T) {
				if identity.Validate() == nil {
					t.Fatal("an invalid signer identity was accepted")
				}
			})
		}
	})
}

// fixtureSignatureCheck gives a check whose verifier gives both SBOM
// attestations of the fixture image by the trusted signer.
func fixtureSignatureCheck(t *testing.T, record *ImageRecord, sboms []SBOM) SignatureCheck {
	t.Helper()
	options := signFixtureOptions(t, "", "", nil)
	check := options.Check
	check.Verifier = fakeVerifier{check.SPDXBundle: signedSBOM(record, sboms[0]), check.CycloneDXBundle: signedSBOM(record, sboms[1])}
	return check
}

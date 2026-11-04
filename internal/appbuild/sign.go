package appbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SignerIdentity is who made an image signature: a keyless identity that the
// signing certificate states, an OIDC issuer and a pipeline definition subject
// with the exact reference it ran at, or a Tekton Chains signer named by the
// digest of its public key, and never both (ST-VO50-C01).
type SignerIdentity struct {
	Issuer          string `json:"issuer,omitempty"`
	Subject         string `json:"subject,omitempty"`
	PublicKeySHA256 string `json:"public_key_sha256,omitempty"`
}

var keylessSubject = regexp.MustCompile(`^https://[^\s@]+@refs/[^\s@]+$`)

// Validate refuses an identity that is neither exactly keyless nor exactly a
// key, or that is both.
func (identity SignerIdentity) Validate() error {
	keyless := identity.Issuer != "" || identity.Subject != ""
	key := identity.PublicKeySHA256 != ""
	switch {
	case keyless && key:
		return errors.New("a signer identity is keyless or a key, never both")
	case key:
		if !hashPattern.MatchString(identity.PublicKeySHA256) {
			return errors.New("a key signer is named by the SHA-256 digest of its public key")
		}
	case keyless:
		if !strings.HasPrefix(identity.Issuer, "https://") || strings.ContainsAny(identity.Issuer, " \t\n") || !keylessSubject.MatchString(identity.Subject) {
			return errors.New("a keyless signer names an HTTPS OIDC issuer and a pipeline definition with the exact reference it ran at")
		}
	default:
		return errors.New("a signer identity is required")
	}
	return nil
}

// VerifiedAttestation is what a signature check gives for one signed
// attestation: the digests its statement is about, its predicate, who signed
// it, the transparency log entry that holds it (-1 for none) and when it was
// signed. Only an AttestationVerifier makes one, after the signature verified.
type VerifiedAttestation struct {
	Subjects      []string
	PredicateType string
	Predicate     json.RawMessage
	Signer        SignerIdentity
	LogIndex      int64
	SignedAt      time.Time
}

// AttestationVerifier checks one signature bundle against the file it signs
// and the trusted signer, and returns the attestation only when the signature
// verifies.
type AttestationVerifier interface {
	VerifyAttestation(ctx context.Context, bundle, subject, predicateType string, signer SignerIdentity) (VerifiedAttestation, error)
}

// SignedAttestation is one SBOM attestation that an image signature holds.
type SignedAttestation struct {
	Format          SBOMFormat `json:"format"`
	PredicateType   string     `json:"predicate_type"`
	PredicateSHA256 string     `json:"predicate_sha256"`
	BundleSHA256    string     `json:"bundle_sha256"`
}

// ImageSignature is the signature record of an image: the image record and
// image it is about, who signed, its entry in the transparency log, when it was
// signed, and its SBOM attestations in the fixed format order.
type ImageSignature struct {
	Format               int                 `json:"format"`
	ImageRecordSHA256    string              `json:"image_record_sha256"`
	ImageSHA256          string              `json:"image_sha256"`
	Signer               SignerIdentity      `json:"signer"`
	TransparencyLogEntry string              `json:"transparency_log_entry"`
	SignedAt             string              `json:"signed_at"`
	Attestations         []SignedAttestation `json:"attestations"`
}

// signedByTrustedSigner is the rule of ST-BLD-INV10: only the pipeline's
// trusted signer signs. It refuses any identity that is not exactly the
// trusted one.
func signedByTrustedSigner(signer, trusted SignerIdentity) error {
	if err := trusted.Validate(); err != nil {
		return fmt.Errorf("the trusted signer: %w", err)
	}
	if signer != trusted {
		return fmt.Errorf("the signature is by %s, not by the trusted signer %s", signer.describe(), trusted.describe())
	}
	return nil
}

func (identity SignerIdentity) describe() string {
	if identity.PublicKeySHA256 != "" {
		return "key sha256:" + identity.PublicKeySHA256
	}
	return identity.Subject + " (" + identity.Issuer + ")"
}

// AcceptableSignature is the predicate of ST-BLD-SP02. An image signature is
// acceptable when it is about the image digest, it is in the transparency log
// or made by a trusted key, and its signer is the trusted signer.
func AcceptableSignature(attestation VerifiedAttestation, trusted SignerIdentity, imageSHA256 string) error {
	if !hashPattern.MatchString(imageSHA256) {
		return errors.New("an image digest is required")
	}
	if len(attestation.Subjects) != 1 || attestation.Subjects[0] != "sha256:"+imageSHA256 {
		return fmt.Errorf("the signature is about %v, not exactly the image sha256:%s", attestation.Subjects, imageSHA256)
	}
	if err := signedByTrustedSigner(attestation.Signer, trusted); err != nil {
		return err
	}
	if attestation.Signer.PublicKeySHA256 == "" && attestation.LogIndex < 0 {
		return errors.New("a keyless signature is not in the transparency log")
	}
	if attestation.SignedAt.IsZero() {
		return errors.New("the signature has no verified signing time")
	}
	return nil
}

// carriesBothSBOMs is the rule of ST-BLD-INV09: the image carries an SPDX and
// a CycloneDX SBOM, both written from its build record and attached as signed
// attestations. The attestations must be exactly one per format, in the fixed
// order, each with the predicate that WriteSBOMs gives for the format.
func carriesBothSBOMs(attestations []VerifiedAttestation, sboms []SBOM) error {
	if len(sboms) != len(sbomFormats) || len(attestations) != len(sbomFormats) {
		return errors.New("an image must carry exactly an SPDX and a CycloneDX SBOM attestation")
	}
	for index, format := range sbomFormats {
		if sboms[index].Format != format || attestations[index].PredicateType != format.PredicateType() {
			return fmt.Errorf("the %s SBOM attestation is missing", format)
		}
		var signed, expected any
		if json.Unmarshal(attestations[index].Predicate, &signed) != nil || json.Unmarshal(sboms[index].Document, &expected) != nil || !reflect.DeepEqual(signed, expected) {
			return fmt.Errorf("the signed %s SBOM differs from the SBOM of the build record", format)
		}
	}
	return nil
}

// SignatureCheck is what checks an image signature: the provider's signature
// bundle of each SBOM attestation, the trusted signer that the delivery
// pipeline names, and the verifier of the provider's signatures.
type SignatureCheck struct {
	SPDXBundle, CycloneDXBundle string
	Signer                      SignerIdentity
	Verifier                    AttestationVerifier
}

// validate refuses a check with an input missing, and names the input, so a
// caller can refuse before it reads the image or opens a connection.
func (check SignatureCheck) validate() error {
	if check.SPDXBundle == "" {
		return errors.New("the signature check needs the SPDX attestation bundle")
	}
	if check.CycloneDXBundle == "" {
		return errors.New("the signature check needs the CycloneDX attestation bundle")
	}
	if err := check.Signer.Validate(); err != nil {
		return fmt.Errorf("the trusted signer: %w", err)
	}
	if check.Verifier == nil {
		return errors.New("the signature check needs the provider's signature verifier")
	}
	return nil
}

// checkImageSignature checks both SBOM attestation bundles about the image:
// each verifies, is about the manifest that the file manifest holds, is
// acceptable (ST-BLD-SP02), and carries the SBOM that WriteSBOMs gives from
// the build record (ST-BLD-INV09). It gives the signature record.
func checkImageSignature(ctx context.Context, check SignatureCheck, manifest string, record *ImageRecord, recordSHA256 string, build *Record) (*ImageSignature, error) {
	if err := check.validate(); err != nil {
		return nil, err
	}
	if build.Artifact != record.Application {
		return nil, errors.New("the image record differs from its build record")
	}
	sboms, err := WriteSBOMs(build)
	if err != nil {
		return nil, err
	}
	bundles := []string{check.SPDXBundle, check.CycloneDXBundle}
	attestations := make([]VerifiedAttestation, 0, len(bundles))
	signature := &ImageSignature{Format: 1, ImageRecordSHA256: recordSHA256, ImageSHA256: record.Manifest.SHA256, Signer: check.Signer}
	for index, format := range sbomFormats {
		bundle, err := readImageBytes(bundles[index], 4<<20)
		if err != nil {
			return nil, fmt.Errorf("the %s signature bundle: %w", format, err)
		}
		attestation, err := check.Verifier.VerifyAttestation(ctx, bundles[index], manifest, format.PredicateType(), check.Signer)
		if err != nil {
			return nil, fmt.Errorf("the %s signature does not verify: %w", format, err)
		}
		if err := AcceptableSignature(attestation, check.Signer, record.Manifest.SHA256); err != nil {
			return nil, fmt.Errorf("the %s signature is not acceptable: %w", format, err)
		}
		attestations = append(attestations, attestation)
		signature.Attestations = append(signature.Attestations, SignedAttestation{Format: format, PredicateType: attestation.PredicateType, PredicateSHA256: sboms[index].Digest, BundleSHA256: digest(bundle)})
	}
	if err := carriesBothSBOMs(attestations, sboms); err != nil {
		return nil, err
	}
	first := attestations[0]
	signature.SignedAt = first.SignedAt.UTC().Format(time.RFC3339)
	if first.LogIndex >= 0 {
		signature.TransparencyLogEntry = "rekor:" + strconv.FormatInt(first.LogIndex, 10)
	}
	return signature, ctx.Err()
}

// SignOptions selects an image folder that BuildImage wrote, the trusted
// digest of its image record, and the check of the provider's signature.
type SignOptions struct {
	Image, RecordSHA256 string
	Check               SignatureCheck
}

// SignImage carries out Sign the image where the CI provider signs for itself
// (ST-BLD-CMD05): the provider's trusted signer has attested both SBOMs about
// the image digest, and STEGO checks that signature. It refuses a missing input
// first, reads the image record at its trusted digest, checks the image
// against it, checks that the SBOM files the image carries are the SBOMs of
// its build record, checks the signature, and only then writes signature.json
// in the image folder. It writes nothing when a check fails.
func SignImage(ctx context.Context, options SignOptions) (*ImageSignature, error) {
	if ctx == nil {
		return nil, errors.New("image signing requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, errors.New("image checks require Linux amd64")
	}
	if options.Image == "" {
		return nil, errors.New("image signing needs the image folder")
	}
	if !hashPattern.MatchString(options.RecordSHA256) {
		return nil, errors.New("image signing needs the trusted image record digest")
	}
	if err := options.Check.validate(); err != nil {
		return nil, err
	}
	record, err := readTrustedImageRecord(filepath.Join(options.Image, "image.json"), options.RecordSHA256)
	if err != nil {
		return nil, err
	}
	if err := verifyImageContents(ctx, filepath.Join(options.Image, "oci"), record, ""); err != nil {
		return nil, err
	}
	build, err := readBuildRecord(filepath.Join(options.Image, "build.json"), record.BuildRecordSHA256)
	if err != nil {
		return nil, err
	}
	sboms, err := WriteSBOMs(build)
	if err != nil {
		return nil, err
	}
	for _, sbom := range sboms {
		carried, err := readImageBytes(filepath.Join(options.Image, sbom.Format.FileName()), 64<<20)
		if err != nil || !bytes.Equal(carried, sbom.Document) {
			return nil, fmt.Errorf("the image's %s SBOM differs from the SBOM of its build record", sbom.Format)
		}
	}
	manifest := filepath.Join(options.Image, "oci", "blobs", "sha256", record.Manifest.SHA256)
	signature, err := checkImageSignature(ctx, options.Check, manifest, record, options.RecordSHA256, build)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(signature, "", "  ")
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(options.Image, "signature.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, writeErr := file.Write(append(data, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return signature, nil
}

// GitHubAttestationVerifier checks a GitHub Actions signature bundle with the
// GitHub CLI, given by its absolute path, as the repository's CI scripts do:
// against the exact signer identity and OIDC issuer, the predicate type and
// the source repository, and refusing self-hosted runners.
type GitHubAttestationVerifier struct {
	GH, Repository string
}

// VerifyAttestation runs gh attestation verify for one bundle and reads the
// verified result.
func (verifier GitHubAttestationVerifier) VerifyAttestation(ctx context.Context, bundle, subject, predicateType string, signer SignerIdentity) (VerifiedAttestation, error) {
	if !filepath.IsAbs(verifier.GH) || !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(verifier.Repository) {
		return VerifiedAttestation{}, errors.New("select the GitHub CLI by its absolute path and the source repository as owner/name")
	}
	if signer.PublicKeySHA256 != "" {
		return VerifiedAttestation{}, errors.New("GitHub signs keyless only")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, verifier.GH, "attestation", "verify", subject, "--bundle", bundle,
		"--hostname", "github.com", "--repo", verifier.Repository,
		"--cert-identity", signer.Subject, "--cert-oidc-issuer", signer.Issuer,
		"--predicate-type", predicateType, "--digest-alg", "sha256",
		"--deny-self-hosted-runners", "--format", "json")
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + os.TempDir(), "GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1"}
	var output, failure boundedBuffer
	output.limit, failure.limit = 4<<20, 64<<10
	command.Stdout, command.Stderr = &output, &failure
	if err := command.Run(); err != nil {
		return VerifiedAttestation{}, fmt.Errorf("gh attestation verify refused the bundle: %s", strings.TrimSpace(failure.String()))
	}
	return readGitHubVerification(output.Bytes())
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	if buffer.Len()+len(data) > buffer.limit {
		return 0, errors.New("command output is too large")
	}
	return buffer.Buffer.Write(data)
}

// readGitHubVerification reads the one verified attestation from the JSON that
// gh attestation verify --format json prints.
func readGitHubVerification(data []byte) (VerifiedAttestation, error) {
	var results []struct {
		Attestation struct {
			Bundle struct {
				VerificationMaterial struct {
					TlogEntries []struct {
						LogIndex json.Number `json:"logIndex"`
					} `json:"tlogEntries"`
				} `json:"verificationMaterial"`
			} `json:"bundle"`
		} `json:"attestation"`
		VerificationResult struct {
			Statement struct {
				Type    string `json:"_type"`
				Subject []struct {
					Digest map[string]string `json:"digest"`
				} `json:"subject"`
				PredicateType string          `json:"predicateType"`
				Predicate     json.RawMessage `json:"predicate"`
			} `json:"statement"`
			Signature struct {
				Certificate struct {
					Issuer                 string `json:"issuer"`
					SubjectAlternativeName string `json:"subjectAlternativeName"`
				} `json:"certificate"`
			} `json:"signature"`
			VerifiedTimestamps []struct {
				Type      string    `json:"type"`
				Timestamp time.Time `json:"timestamp"`
			} `json:"verifiedTimestamps"`
		} `json:"verificationResult"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&results); err != nil {
		return VerifiedAttestation{}, errors.New("gh attestation verify printed no verification result")
	}
	if len(results) != 1 {
		return VerifiedAttestation{}, fmt.Errorf("the bundle holds %d verified attestations, not one", len(results))
	}
	result := results[0]
	statement := result.VerificationResult.Statement
	if statement.Type != "https://in-toto.io/Statement/v1" {
		return VerifiedAttestation{}, fmt.Errorf("the signed statement is of type %q, not an in-toto statement, version 1", statement.Type)
	}
	attestation := VerifiedAttestation{PredicateType: statement.PredicateType, Predicate: statement.Predicate, LogIndex: -1}
	for _, subject := range statement.Subject {
		attestation.Subjects = append(attestation.Subjects, "sha256:"+subject.Digest["sha256"])
	}
	certificate := result.VerificationResult.Signature.Certificate
	attestation.Signer = SignerIdentity{Issuer: certificate.Issuer, Subject: certificate.SubjectAlternativeName}
	if entries := result.Attestation.Bundle.VerificationMaterial.TlogEntries; len(entries) > 0 {
		index, err := entries[0].LogIndex.Int64()
		if err != nil || index < 0 {
			return VerifiedAttestation{}, errors.New("the transparency log index is not valid")
		}
		attestation.LogIndex = index
	}
	for _, stamp := range result.VerificationResult.VerifiedTimestamps {
		if stamp.Type == "Tlog" && !stamp.Timestamp.IsZero() {
			attestation.SignedAt = stamp.Timestamp
			break
		}
	}
	return attestation, nil
}

package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/jsell-rh/stego/internal/appbuild"
)

// signatureFlags adds the flags that check an image signature to a flag set:
// both SBOM attestation bundles, the trusted signer, the verifier and the
// source repository. The function it gives refuses a missing flag by name.
func signatureFlags(flags *flag.FlagSet) func() (appbuild.SignatureCheck, error) {
	values := map[string]*string{}
	for _, item := range []struct{ name, usage string }{
		{"spdx-bundle", "provider signature bundle of the SPDX SBOM attestation"},
		{"cyclonedx-bundle", "provider signature bundle of the CycloneDX SBOM attestation"},
		{"signer-issuer", "OIDC issuer of the trusted signer"},
		{"signer-subject", "trusted signer workflow identity at its exact reference"},
		{"gh", "absolute path of the GitHub CLI that verifies the bundles"},
		{"source-repository", "source repository of the signed run, as owner/name"},
	} {
		values[item.name] = flags.String(item.name, "", item.usage)
	}
	return func() (appbuild.SignatureCheck, error) {
		for _, name := range []string{"spdx-bundle", "cyclonedx-bundle", "signer-issuer", "signer-subject", "gh", "source-repository"} {
			if *values[name] == "" {
				return appbuild.SignatureCheck{}, fmt.Errorf("%s requires --%s", flags.Name(), name)
			}
		}
		return appbuild.SignatureCheck{
			SPDXBundle:      *values["spdx-bundle"],
			CycloneDXBundle: *values["cyclonedx-bundle"],
			Signer:          appbuild.SignerIdentity{Issuer: *values["signer-issuer"], Subject: *values["signer-subject"]},
			Verifier:        appbuild.GitHubAttestationVerifier{GH: *values["gh"], Repository: *values["source-repository"]},
		}, nil
	}
}

// runImageSign parses stego image sign (ST-BLD-CLI05) and checks the
// signature that the CI provider made for the image, through
// appbuild.SignImage, which writes signature.json. It refuses a missing flag
// by name before it reads the image.
func runImageSign(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("image sign", flag.ContinueOnError)
	image := flags.String("image", "", "image folder that stego image build wrote")
	recordSHA256 := flags.String("record-sha256", "", "trusted digest of the image record")
	check := signatureFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("image sign accepts no positional arguments")
	}
	if *image == "" {
		return fmt.Errorf("image sign requires --image")
	}
	if *recordSHA256 == "" {
		return fmt.Errorf("image sign requires --record-sha256")
	}
	selected, err := check()
	if err != nil {
		return err
	}
	signature, err := appbuild.SignImage(ctx, appbuild.SignOptions{Image: *image, RecordSHA256: *recordSHA256, Check: selected})
	if err != nil {
		return err
	}
	fmt.Printf("image sha256:%s signed by %s; transparency log entry %s\n", signature.ImageSHA256, signature.Signer.Subject, signature.TransparencyLogEntry)
	for _, attestation := range signature.Attestations {
		fmt.Printf("%s attestation: bundle sha256:%s, SBOM sha256:%s\n", attestation.Format, attestation.BundleSHA256, attestation.PredicateSHA256)
	}
	return nil
}

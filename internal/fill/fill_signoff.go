package fill

import (
	"errors"
	"fmt"
	"time"
)

// FillSignOff is a person's signature, in the fill manifest, of the fill's
// test evidence (ST-EXT-EN03). It vouches that a person lets this exact fill
// ship.
type FillSignOff struct {
	EvidenceDigest string    `yaml:"evidence_digest"`
	Signature      Signature `yaml:"signature"`
	SignedAt       time.Time `yaml:"signed_at"`
}

// check refuses a sign-off with a missing field, and names the field.
func (s *FillSignOff) check() error {
	if s.EvidenceDigest == "" {
		return errors.New("missing required field: evidence_digest")
	}
	if !validDigest(s.EvidenceDigest) {
		return fmt.Errorf("evidence_digest %q is not 64 lower-case hexadecimal digits", s.EvidenceDigest)
	}
	if s.Signature.Armored == "" {
		return errors.New("missing required field: signature.signature")
	}
	if err := s.Signature.check(); err != nil {
		return err
	}
	if s.SignedAt.IsZero() {
		return errors.New("missing required field: signed_at")
	}
	return nil
}

// WriteSignOff writes the sign-off into the fill manifest at path, in place
// of the sign-off it held, and leaves the rest of the manifest as it was.
func WriteSignOff(path string, signOff FillSignOff) error {
	if err := signOff.check(); err != nil {
		return err
	}
	return writeSection(path, "sign_off", signOff)
}

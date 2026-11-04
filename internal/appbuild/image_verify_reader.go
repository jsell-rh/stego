package appbuild

import "errors"

// ImageVerification is the read model Image verification (ST-BLD-RM02): the
// image record that the image matches, its trusted build record, and its
// acceptable signature. It exists only when all three checks passed.
type ImageVerification struct {
	Record    *ImageRecord
	Build     *Record
	Signature *ImageSignature
}

// ImageVerifyReader holds the image verification value that the run built in
// memory and hands it to the query handler; nothing is kept between runs.
type ImageVerifyReader struct {
	verification *ImageVerification
}

func (reader *ImageVerifyReader) hold(verification ImageVerification) {
	reader.verification = &verification
}

// Verification gives the held verification, and refuses one that is missing
// or lacks any of its three checks.
func (reader *ImageVerifyReader) Verification() (*ImageVerification, error) {
	if reader.verification == nil || reader.verification.Record == nil || reader.verification.Build == nil || reader.verification.Signature == nil {
		return nil, errors.New("the image verification is not complete")
	}
	held := *reader.verification
	return &held, nil
}

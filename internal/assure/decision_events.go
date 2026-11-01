package assure

// ChangeDecided is the event of ST-ASR-EV02, Change decided. GateChange returns it in process when the change
// gate reached its decision on a change and the decision store kept it. It names the change's commit, the base
// head, whether the change passed, and every reason for a refusal.
type ChangeDecided struct {
	Commit     string
	BaseCommit string
	Passed     bool
	Reasons    []string
}

// ReleaseDecided is the event of ST-ASR-EV03, Release decided. GateRelease returns it in process when the release
// gate reached its decision on a release commit and the decision store kept it. It names the commit, whether the
// release passed, and every reason for a refusal. For a passed decision it also names the attestation the
// provider's trusted signer is to make and the digest of the decision.json predicate handed over for it; a
// refused decision has neither. Release qualification and publishing read the attestation the provider keeps,
// never this value.
type ReleaseDecided struct {
	Commit          string
	Passed          bool
	Reasons         []string
	Attestation     *Attestation
	PredicateDigest string
}

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

// ReleaseDecided is the event of ST-ASR-EV03, Release decided: the release gate reached its decision on a
// release commit, and a passed decision is attested by the provider's trusted signer. Gate a release
// (ST-ASR-UC03) raises it; release qualification reads the attestation, never this value.
type ReleaseDecided struct {
	Commit      string
	Passed      bool
	Attestation Attestation
}

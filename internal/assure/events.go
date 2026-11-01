package assure

// ProofRunAttested is the event of ST-ASR-EV01, Proof run attested. RecordProofRun returns it in process when the
// store has handed the proof run's statement to the provider's attestation action, which signs it with the
// commit as its subject after the command exits. It names the commit, the digest of the spec the outcomes
// answer, the attestation reference the provider's trusted signer makes, and the digest of the predicate it
// signs. The gates read the attestation the provider keeps, never this value.
type ProofRunAttested struct {
	Commit          string
	SpecDigest      string
	Attestation     Attestation
	PredicateDigest string
}

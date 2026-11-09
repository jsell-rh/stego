package types

// Audit is the audit setting of a declaration (ST-DECL-EN10): how the service records what happened. A
// declaration that omits it gets audit at level change, with a SHA-256 chain and an anchor.
type Audit struct {
	// Enabled says whether audit is on. Omitted means on; off needs a reason.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Sink is the port binding that the audit records are copied to through the outbox.
	Sink string `yaml:"sink,omitempty"`
	// Retention is how long the local audit partitions are kept, such as 13 months.
	Retention string `yaml:"retention,omitempty"`
	// Level is metadata or change. Omitted means change.
	Level string `yaml:"level,omitempty"`
	// Reason is why audit is off.
	Reason string `yaml:"reason,omitempty"`
	// PII is redact or keep. Omitted means redact.
	PII string `yaml:"pii,omitempty"`
}

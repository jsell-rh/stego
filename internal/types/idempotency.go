package types

// Idempotency is the idempotency setting of a declaration (ST-DECL-EN13): the operations a request must carry an
// Idempotency-Key for, how long a key and its stored response are kept, and what the fingerprint covers besides
// the method and the path. Checking holds Required on to ST-DECL-INV25.
type Idempotency struct {
	RequiredOn  []Operation `yaml:"required_on"`
	Retention   string      `yaml:"retention,omitempty"`
	Fingerprint string      `yaml:"fingerprint,omitempty"`
}

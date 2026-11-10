package types

// ExperimentalAcceptance is one statement that the service accepts a component that is not ga, with the date the
// acceptance runs out and the reason (ST-DECL-EN07). It is the only way an experiment reaches a service, and it
// expires: it holds to the end of AcceptedUntil in UTC.
type ExperimentalAcceptance struct {
	// Component names the component the service accepts, whose stage is not ga.
	Component string `yaml:"component"`
	// AcceptedUntil is the date the acceptance runs out, such as 2027-03-01.
	AcceptedUntil string `yaml:"accepted_until"`
	// Reason is why the service accepts it, so a reader can judge the risk.
	Reason string `yaml:"reason"`
}

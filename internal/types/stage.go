package types

import "fmt"

// Stage is how far a component is on the maturity ladder: alpha, beta or ga.
// It decides whether a golden path may include the component.
type Stage string

const (
	// StageAlpha is the first rung: an experiment with a sunset date.
	StageAlpha Stage = "alpha"
	// StageBeta is the second rung: an experiment with a sunset date.
	StageBeta Stage = "beta"
	// StageGA is generally available: fit for a golden path, with no sunset.
	StageGA Stage = "ga"
)

// ParseStage gives the stage a value names, and refuses any value that is not
// exactly alpha, beta or ga, naming the value and the three stages.
func ParseStage(value string) (Stage, error) {
	switch Stage(value) {
	case StageAlpha, StageBeta, StageGA:
		return Stage(value), nil
	}
	return "", fmt.Errorf("maturity stage %q is not valid; it must be %s, %s or %s", value, StageAlpha, StageBeta, StageGA)
}

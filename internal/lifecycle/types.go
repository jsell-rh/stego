// Package lifecycle keeps the digest lifecycle of Distributing (ST-DIS-A2): what happened to one component digest,
// or one runtime library version, after the registry published it. A supersession names its successor and the
// date after which the digest fails; a withdrawal says that the digest must not be used any more. The records are
// OSV advisories in the lifecycle folder of the registry, read from the registry's current head and not from the
// snapshot a service pins, so a consumer on an old snapshot still learns of them.
package lifecycle

import (
	"fmt"
	"regexp"
	"time"
)

// The kinds of package a lifecycle is about.
const (
	// ComponentPackage is a component of the registry, pinned by its component digest.
	ComponentPackage = "component"
	// RuntimePackage is the runtime library, pinned by its tagged version.
	RuntimePackage = "runtime"
)

// Subject is the identity of one lifecycle: the kind of package, the component name or the runtime library's
// module path, and the component digest or the runtime version.
type Subject struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// String names the subject as a report shows it.
func (s Subject) String() string {
	return s.Name + "@" + s.Digest
}

var (
	componentDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	runtimeVersion  = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	packageName     = regexp.MustCompile(`^[a-z0-9][a-z0-9./_-]*$`)
)

// check refuses a subject that is not a component with a digest of 64 lower-case hexadecimal digits, or the
// runtime library with a tagged version.
func (s Subject) check(role string) error {
	if s.Name == "" {
		return fmt.Errorf("%s: the component name is missing", role)
	}
	if !packageName.MatchString(s.Name) {
		return fmt.Errorf("%s: %q is not a component name or a module path", role, s.Name)
	}
	switch s.Kind {
	case ComponentPackage:
		if s.Digest == "" {
			return fmt.Errorf("%s: the digest is missing", role)
		}
		if !componentDigest.MatchString(s.Digest) {
			return fmt.Errorf("%s: %q is not a component digest of 64 lower-case hexadecimal digits", role, s.Digest)
		}
	case RuntimePackage:
		if s.Digest == "" {
			return fmt.Errorf("%s: the runtime version is missing", role)
		}
		if !runtimeVersion.MatchString(s.Digest) {
			return fmt.Errorf("%s: %q is not a tagged runtime version such as v1.2.3", role, s.Digest)
		}
	default:
		return fmt.Errorf("%s: the kind %q is neither %s nor %s", role, s.Kind, ComponentPackage, RuntimePackage)
	}
	return nil
}

// Lifecycle is the standing of one published digest (ST-DIS-EN06): its supersession and its withdrawal, each at
// most once.
type Lifecycle struct {
	Subject      Subject
	Supersession *Supersession
	Withdrawal   *Withdrawal
}

// Supersession states that the digest has a successor, with the date after which the digest fails
// (ST-DIS-EN07).
type Supersession struct {
	// Successor is the digest that replaces this one: another digest of the same component, or a digest of the
	// component that graduated to ga and took over the port this component provided.
	Successor Subject
	// FailsAfter is the last day on which a pin to the superseded digest still stands.
	FailsAfter time.Time
	// NeedsDeclarationChange tells whether taking the successor needs a change to the service declaration.
	NeedsDeclarationChange bool
	// NeedsFillChange tells whether taking the successor needs a change to a fill.
	NeedsFillChange bool
	// RecordedOn is when the platform engineer recorded the supersession.
	RecordedOn time.Time
	// AdvisoryID is the OSV id of the advisory, unique in the registry.
	AdvisoryID string
	// Summary says what the successor changes, in one line.
	Summary string
	// References are links to the change, its review or its release notes.
	References []string
}

// Withdrawal states that the digest must not be used any more, with the date after which it fails
// (ST-DIS-EN08).
type Withdrawal struct {
	// FailsAfter is the last day on which a pin that a service already holds still stands.
	FailsAfter time.Time
	// Reason is why the digest is withdrawn, for the people who pin it.
	Reason string
	// RecordedOn is when the platform engineer recorded the withdrawal; it is also the OSV withdrawn timestamp.
	RecordedOn time.Time
	// AdvisoryID is the OSV id of the advisory, unique in the registry.
	AdvisoryID string
	// References are links to the finding that caused the withdrawal.
	References []string
}

// dateLayout is how a fail date is written: a calendar day.
const dateLayout = "2006-01-02"

// Day gives the calendar day of a time, in UTC, as midnight of that day.
func Day(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// ParseDate reads a fail date written as YYYY-MM-DD.
func ParseDate(text string) (time.Time, error) {
	day, err := time.Parse(dateLayout, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a date written as YYYY-MM-DD", text)
	}
	return day, nil
}

// FormatDate writes a fail date as YYYY-MM-DD.
func FormatDate(day time.Time) string {
	return Day(day).Format(dateLayout)
}

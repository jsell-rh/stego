package lifecycle

import (
	"time"
)

// IsSuperseded is the predicate of ST-DIS-SP02: a digest is superseded when its lifecycle holds a supersession.
func IsSuperseded(l Lifecycle) bool {
	return l.Supersession != nil
}

// IsPastFailDate is the predicate of ST-DIS-SP03: a digest is past its fail date when its lifecycle holds a
// supersession or a withdrawal and the date after which it fails has passed on the given day. On the fail date
// itself the digest is not past it.
func IsPastFailDate(l Lifecycle, day time.Time) bool {
	today := Day(day)
	if l.Supersession != nil && today.After(Day(l.Supersession.FailsAfter)) {
		return true
	}
	return l.Withdrawal != nil && today.After(Day(l.Withdrawal.FailsAfter))
}

// IsWithdrawn is the predicate of ST-DIS-SP04: a digest is withdrawn when its lifecycle holds a withdrawal,
// whatever the date of the withdrawal.
func IsWithdrawn(l Lifecycle) bool {
	return l.Withdrawal != nil
}

// Standing is the read model Pin standing (ST-DIS-RM03) as one run builds it in memory: each pin of one service
// with the lifecycle of its digest on one day, or the reason the lifecycle records could not be read. Nothing is
// kept between runs.
type Standing struct {
	// Day is the day the standing is for.
	Day time.Time
	// Pins are the pins of the service, each with the lifecycle of its digest.
	Pins []Lifecycle
	// Unreadable is why the lifecycle records could not be read; then no pin is known to be current.
	Unreadable error
}

// Project builds the standing of the pins from the lifecycles the store read, or from the error that kept it
// from reading them.
func Project(pins []Subject, lifecycles []Lifecycle, unreadable error, day time.Time) Standing {
	standing := Standing{Day: Day(day), Unreadable: unreadable}
	bySubject := map[Subject]Lifecycle{}
	for _, l := range lifecycles {
		bySubject[l.Subject] = l
	}
	for _, pin := range pins {
		l, held := bySubject[pin]
		if !held {
			l = Lifecycle{Subject: pin}
		}
		standing.Pins = append(standing.Pins, l)
	}
	return standing
}

// PinState is the standing of one pin, as validate, plan and apply report it.
type PinState struct {
	Pin Subject
	// Unchecked tells that the lifecycle records could not be read, so the pin is not known to be current.
	Unchecked bool
	// Cause is why the pin is unchecked.
	Cause string
	// Superseded and Supersession are the pin's supersession, when it has one.
	Superseded   bool
	Supersession *Supersession
	// Withdrawn and Withdrawal are the pin's withdrawal, when it has one.
	Withdrawn  bool
	Withdrawal *Withdrawal
	// PastFailDate tells that the supersession's or the withdrawal's fail date has passed.
	PastFailDate bool
}

// Current tells that the pin is known to be neither superseded nor withdrawn.
func (p PinState) Current() bool {
	return !p.Unchecked && !p.Superseded && !p.Withdrawn
}

// PinStanding is the query List the standing of each pin (ST-DIS-QRY03): each pin of the service with its
// standing, superseded with its successor and fail date, withdrawn with its fail date, or unchecked when the
// lifecycle records cannot be read.
func PinStanding(reader *StandingReader) []PinState {
	standing := reader.Standing()
	states := make([]PinState, 0, len(standing.Pins))
	for _, l := range standing.Pins {
		state := PinState{Pin: l.Subject}
		if standing.Unreadable != nil {
			state.Unchecked = true
			state.Cause = standing.Unreadable.Error()
			states = append(states, state)
			continue
		}
		if IsSuperseded(l) {
			state.Superseded, state.Supersession = true, l.Supersession
		}
		if IsWithdrawn(l) {
			state.Withdrawn, state.Withdrawal = true, l.Withdrawal
		}
		state.PastFailDate = IsPastFailDate(l, standing.Day)
		states = append(states, state)
	}
	return states
}

// ReadStanding reads the lifecycle folder of the store once and gives the standing of each pin on the day.
func ReadStanding(store *Store, pins []Subject, day time.Time) []PinState {
	all, err := store.All()
	return PinStanding(NewStandingReader(Project(pins, all, err, day)))
}

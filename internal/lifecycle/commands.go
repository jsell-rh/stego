package lifecycle

import (
	"fmt"
	"strings"
	"time"
)

// SupersedeRequest is the command Supersede a component digest (ST-DIS-CMD03), with the registry's facts about
// both components that its rules read.
type SupersedeRequest struct {
	// Superseded is the digest that gets a successor.
	Superseded Subject
	// SupersededStage is the stage of the superseded component in the registry; its window bounds the fail date.
	SupersededStage string
	// SupersededProvides are the ports the superseded component provides.
	SupersededProvides []string
	// Successor is the declared successor digest.
	Successor Subject
	// SuccessorStage is the stage of the successor's component.
	SuccessorStage string
	// SuccessorTookOver are the ports the successor's component took over from another component.
	SuccessorTookOver []string
	// SuccessorReleasedOn is the day the registry released the successor.
	SuccessorReleasedOn time.Time
	// FailsAfter is the fail date the platform engineer gives; nil takes the latest date the window allows.
	FailsAfter *time.Time
	// NeedsDeclarationChange and NeedsFillChange must both be stated: a digest carries no compatibility meaning.
	NeedsDeclarationChange *bool
	NeedsFillChange        *bool
	// Summary says what the successor changes, in one line.
	Summary string
	// References are links to the change, its review or its release notes.
	References []string
	// RecordedOn is when the supersession is recorded.
	RecordedOn time.Time
	// Windows are the registry's windows per stage; nil takes DefaultWindows.
	Windows Windows
}

// WithdrawRequest is the command Withdraw a component digest (ST-DIS-CMD04).
type WithdrawRequest struct {
	// Withdrawn is the digest that must not be used any more.
	Withdrawn Subject
	// FailsAfter is the future day after which a pin that a service holds fails.
	FailsAfter *time.Time
	// Reason is why the digest is withdrawn.
	Reason string
	// References are links to the finding that caused the withdrawal.
	References []string
	// RecordedOn is when the withdrawal is recorded.
	RecordedOn time.Time
}

// supersede applies a supersession to the lifecycle, or refuses it and leaves the lifecycle as it was.
func (l *Lifecycle) supersede(request SupersedeRequest, advisoryID string) (DigestSuperseded, error) {
	if err := request.Superseded.check("the superseded digest"); err != nil {
		return DigestSuperseded{}, err
	}
	if request.Successor.Digest == "" {
		return DigestSuperseded{}, fmt.Errorf("the successor of %s is missing", request.Superseded)
	}
	if request.Successor.Name == "" {
		request.Successor.Name = request.Superseded.Name
	}
	if request.Successor.Kind == "" {
		request.Successor.Kind = request.Superseded.Kind
	}
	if err := request.Successor.check("the successor"); err != nil {
		return DigestSuperseded{}, err
	}
	if request.NeedsDeclarationChange == nil {
		return DigestSuperseded{}, fmt.Errorf("the supersession of %s does not state whether taking the successor needs a declaration change", request.Superseded)
	}
	if request.NeedsFillChange == nil {
		return DigestSuperseded{}, fmt.Errorf("the supersession of %s does not state whether taking the successor needs a fill change", request.Superseded)
	}
	if strings.TrimSpace(request.Summary) == "" {
		return DigestSuperseded{}, fmt.Errorf("the supersession of %s has no summary", request.Superseded)
	}
	if request.RecordedOn.IsZero() {
		return DigestSuperseded{}, fmt.Errorf("the supersession of %s has no time of recording", request.Superseded)
	}
	if err := oneSuccessor(*l, request); err != nil {
		return DigestSuperseded{}, err
	}
	failsAfter, err := failDateInWindow(request)
	if err != nil {
		return DigestSuperseded{}, err
	}
	l.Subject = request.Superseded
	l.Supersession = &Supersession{
		Successor:              request.Successor,
		FailsAfter:             failsAfter,
		NeedsDeclarationChange: *request.NeedsDeclarationChange,
		NeedsFillChange:        *request.NeedsFillChange,
		RecordedOn:             request.RecordedOn.UTC(),
		AdvisoryID:             advisoryID,
		Summary:                request.Summary,
		References:             append([]string(nil), request.References...),
	}
	return DigestSuperseded{Subject: l.Subject, Supersession: *l.Supersession}, nil
}

// withdraw applies a withdrawal to the lifecycle, or refuses it and leaves the lifecycle as it was.
func (l *Lifecycle) withdraw(request WithdrawRequest, advisoryID string) (DigestWithdrawn, error) {
	if err := request.Withdrawn.check("the withdrawn digest"); err != nil {
		return DigestWithdrawn{}, err
	}
	if request.FailsAfter == nil {
		return DigestWithdrawn{}, fmt.Errorf("the withdrawal of %s has no fail date", request.Withdrawn)
	}
	if strings.TrimSpace(request.Reason) == "" {
		return DigestWithdrawn{}, fmt.Errorf("the withdrawal of %s has no reason", request.Withdrawn)
	}
	if request.RecordedOn.IsZero() {
		return DigestWithdrawn{}, fmt.Errorf("the withdrawal of %s has no time of recording", request.Withdrawn)
	}
	failsAfter, today := Day(*request.FailsAfter), Day(request.RecordedOn)
	if failsAfter.Before(today) {
		return DigestWithdrawn{}, fmt.Errorf("the fail date %s of the withdrawal of %s is already past (today is %s); a withdrawal carries a future date",
			FormatDate(failsAfter), request.Withdrawn, FormatDate(today))
	}
	if l.Withdrawal != nil {
		return DigestWithdrawn{}, fmt.Errorf("%s is already withdrawn (advisory %s)", request.Withdrawn, l.Withdrawal.AdvisoryID)
	}
	l.Subject = request.Withdrawn
	l.Withdrawal = &Withdrawal{
		FailsAfter: failsAfter,
		Reason:     request.Reason,
		RecordedOn: request.RecordedOn.UTC(),
		AdvisoryID: advisoryID,
		References: append([]string(nil), request.References...),
	}
	return DigestWithdrawn{Subject: l.Subject, Withdrawal: *l.Withdrawal}, nil
}

// Supersede records the successor of a component digest or a runtime library version in the lifecycle folder of
// the registry checkout, as one new advisory, and gives the event Component digest superseded. A refused
// supersession writes nothing.
func Supersede(store *Store, request SupersedeRequest) (DigestSuperseded, error) {
	current, err := store.Get(request.Superseded)
	if err != nil {
		return DigestSuperseded{}, err
	}
	id, err := store.nextAdvisoryID(request.RecordedOn)
	if err != nil {
		return DigestSuperseded{}, err
	}
	event, err := current.supersede(request, id)
	if err != nil {
		return DigestSuperseded{}, err
	}
	if err := store.add(supersessionAdvisory(current.Subject, *current.Supersession)); err != nil {
		return DigestSuperseded{}, err
	}
	return event, nil
}

// Withdraw records that a component digest or a runtime library version must not be used any more, as one new
// advisory in the lifecycle folder of the registry checkout, and gives the event Component digest withdrawn. A
// refused withdrawal writes nothing.
func Withdraw(store *Store, request WithdrawRequest) (DigestWithdrawn, error) {
	current, err := store.Get(request.Withdrawn)
	if err != nil {
		return DigestWithdrawn{}, err
	}
	id, err := store.nextAdvisoryID(request.RecordedOn)
	if err != nil {
		return DigestWithdrawn{}, err
	}
	event, err := current.withdraw(request, id)
	if err != nil {
		return DigestWithdrawn{}, err
	}
	if err := store.add(withdrawalAdvisory(current.Subject, *current.Withdrawal)); err != nil {
		return DigestWithdrawn{}, err
	}
	return event, nil
}

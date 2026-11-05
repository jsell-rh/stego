package lifecycle

import (
	"fmt"
	"slices"
	"time"
)

// Windows gives, for each component stage, the months after the successor's release before a superseded digest
// fails: the window the registry states for the stage.
type Windows map[string]int

// DefaultWindows are the windows of ST-DIS-INV12 and ST-DIS-PO01: alpha 0 months, beta 3 months, ga 6 months.
var DefaultWindows = Windows{"alpha": 0, "beta": 3, "ga": 6}

// oneSuccessor is the rule of ST-DIS-INV06: a digest has at most one successor, and the successor is another
// digest of the same component, or a digest of the component that graduated to ga and took over a port the
// superseded component provided, in which case the supersession needs a declaration change.
func oneSuccessor(current Lifecycle, request SupersedeRequest) error {
	superseded, successor := request.Superseded, request.Successor
	if current.Supersession != nil {
		return fmt.Errorf("%s already has the successor %s (advisory %s); a digest has at most one successor",
			superseded, current.Supersession.Successor, current.Supersession.AdvisoryID)
	}
	if successor.Kind != superseded.Kind {
		return fmt.Errorf("the successor %s is a %s and %s is a %s; a successor is of the same kind",
			successor, successor.Kind, superseded, superseded.Kind)
	}
	if successor.Name == superseded.Name {
		if successor.Digest == superseded.Digest {
			return fmt.Errorf("the successor of %s is the same digest; a successor is another digest", superseded)
		}
		return nil
	}
	tookOver := false
	for _, port := range request.SuccessorTookOver {
		if slices.Contains(request.SupersededProvides, port) {
			tookOver = true
		}
	}
	if !tookOver {
		return fmt.Errorf("the successor %s is of the other component %s, which took over no port of %s",
			successor, successor.Name, superseded.Name)
	}
	if request.SuccessorStage != "ga" {
		return fmt.Errorf("the successor %s is of the other component %s at stage %q, which did not graduate to ga",
			successor, successor.Name, request.SuccessorStage)
	}
	if !*request.NeedsDeclarationChange {
		return fmt.Errorf("the successor %s is of the other component %s, so taking it needs a declaration change; state it",
			successor, successor.Name)
	}
	return nil
}

// failDateInWindow is the rule of ST-DIS-INV12: a supersession's fail date is the successor's release date plus
// the window of the superseded component's stage, doubled when taking the successor needs a declaration change,
// unless a platform engineer shortens it; it is never later. With no fail date given, it gives the latest date.
func failDateInWindow(request SupersedeRequest) (time.Time, error) {
	if request.SuccessorReleasedOn.IsZero() {
		return time.Time{}, fmt.Errorf("the release date of the successor %s is missing", request.Successor)
	}
	windows := request.Windows
	if windows == nil {
		windows = DefaultWindows
	}
	months, known := windows[request.SupersededStage]
	if !known {
		return time.Time{}, fmt.Errorf("the registry states no window for the stage %q of %s", request.SupersededStage, request.Superseded.Name)
	}
	if *request.NeedsDeclarationChange {
		months *= 2
	}
	latest := Day(request.SuccessorReleasedOn).AddDate(0, months, 0)
	if request.FailsAfter == nil {
		return latest, nil
	}
	failsAfter := Day(*request.FailsAfter)
	if failsAfter.After(latest) {
		return time.Time{}, fmt.Errorf("the fail date %s is later than the latest date allowed, %s: the release date %s plus %d months for stage %s",
			FormatDate(failsAfter), FormatDate(latest), FormatDate(request.SuccessorReleasedOn), months, request.SupersededStage)
	}
	return failsAfter, nil
}

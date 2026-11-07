package lifecycle

// StandingReader holds the standing value that the run built in memory and hands it to the query handler
// (ST-DIS-RM03-RD); the handler reads nothing else, and nothing is kept between runs.
type StandingReader struct {
	standing Standing
}

// NewStandingReader holds the standing a run built.
func NewStandingReader(standing Standing) *StandingReader {
	return &StandingReader{standing: standing}
}

// Standing gives the standing the reader holds.
func (r *StandingReader) Standing() Standing {
	return r.standing
}

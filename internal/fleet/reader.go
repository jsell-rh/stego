package fleet

// SweepReader holds the sweep value that the run built in memory and hands it to the query handlers
// (ST-DIS-RM01-RD); the handlers read nothing else, and nothing is kept between runs.
type SweepReader struct {
	sweep Sweep
}

// NewSweepReader holds the sweep a run built.
func NewSweepReader(sweep Sweep) *SweepReader {
	return &SweepReader{sweep: sweep}
}

// Sweep gives the sweep the reader holds.
func (r *SweepReader) Sweep() Sweep {
	return r.sweep
}

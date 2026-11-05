package fill

// QualificationReader holds the qualification that the run built in memory
// and hands it to the query handler (ST-EXT-RM02). Nothing is kept between
// runs.
type QualificationReader struct {
	rows []Qualification
}

// NewQualificationReader holds the rows of one run.
func NewQualificationReader(rows []Qualification) *QualificationReader {
	return &QualificationReader{rows: rows}
}

// Rows gives a copy of the rows, in the order the projection built them.
func (r *QualificationReader) Rows() []Qualification {
	out := make([]Qualification, len(r.rows))
	copy(out, r.rows)
	return out
}

package assure

// LedgerReader is the reader of the proof ledger (ST-ASR-RM01-RD). It holds the ledger that BuildLedger built in
// memory for this run (ST-STORE05) and hands it to the query handler, NotPassedAt, which reads nothing else.
// Nothing is kept between runs.
type LedgerReader struct {
	ledger Ledger
}

// NewLedgerReader holds the ledger the run built.
func NewLedgerReader(ledger Ledger) LedgerReader {
	return LedgerReader{ledger: ledger}
}

// Ledger is the ledger the reader holds. Its rows are a copy, so a query handler cannot change what another reads.
func (r LedgerReader) Ledger() Ledger {
	ledger := r.ledger
	ledger.Rows = append([]LedgerRow(nil), r.ledger.Rows...)
	return ledger
}

package fill

// CatalogReader holds the catalog that the run built in memory and hands it
// to the query handler (ST-EXT-RM01). Nothing is kept between runs.
type CatalogReader struct {
	entries []CatalogEntry
}

// NewCatalogReader holds the entries of one run.
func NewCatalogReader(entries []CatalogEntry) *CatalogReader {
	return &CatalogReader{entries: entries}
}

// Entries gives a copy of the entries, in the order the projection built
// them.
func (r *CatalogReader) Entries() []CatalogEntry {
	out := make([]CatalogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

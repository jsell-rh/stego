package declaration

import "encoding/json"

// FaultsReader holds the faults that one check built in memory and hands them to the query handler
// (ST-DECL-RM01-RD). The handler reads nothing else, and nothing is kept between runs.
type FaultsReader struct {
	faults []ValidationError
}

// FaultsDocument is the structured form of the faults of one declaration (ST-DECL-RM01): every fault in the
// order the check found it, and whether the declaration is acceptable, which is when there is no fault.
type FaultsDocument struct {
	Accepted bool              `json:"accepted"`
	Faults   []ValidationError `json:"faults"`
}

// NewFaultsReader reads the faults of one check.
func NewFaultsReader(result *ValidationResult) FaultsReader {
	reader := FaultsReader{faults: []ValidationError{}}
	if result == nil {
		return reader
	}
	for _, fault := range result.Errors {
		if fault.Rule == "" {
			fault.Rule = fault.Category
		}
		if fault.Path == "" {
			fault.Path = "service.yaml"
		}
		reader.faults = append(reader.faults, fault)
	}
	return reader
}

// Document gives the faults as one structured document.
func (r FaultsReader) Document() FaultsDocument {
	return FaultsDocument{Accepted: len(r.faults) == 0, Faults: append([]ValidationError{}, r.faults...)}
}

// JSON gives the structured document as JSON, for an agent to read.
func (r FaultsReader) JSON() ([]byte, error) {
	return json.MarshalIndent(r.Document(), "", "  ")
}

// Prose gives the faults for a person, each rendered from its structured fields.
func (r FaultsReader) Prose() string {
	return FormatValidation(&ValidationResult{Errors: r.faults})
}

package declaration

import "encoding/json"

// FaultsReader holds the faults that one check built in memory and hands them to the query handler
// (ST-DECL-RM01-RD). The handler reads nothing else, and nothing is kept between runs.
type FaultsReader struct {
	faults   []ValidationError
	settings *Settings
}

// FaultsDocument is the structured form of the faults of one declaration (ST-DECL-RM01): every fault in the
// order the check found it, and whether the declaration is acceptable, which is when there is no fault.
type FaultsDocument struct {
	Accepted bool              `json:"accepted"`
	Faults   []ValidationError `json:"faults"`
	// Settings are the declaration's settings with the defaults filled in (ST-DECL-UC05-FL-S02).
	Settings *Settings `json:"settings,omitempty"`
}

// NewFaultsReader reads the faults of one check.
func NewFaultsReader(result *ValidationResult) FaultsReader {
	reader := FaultsReader{faults: []ValidationError{}}
	if result == nil {
		return reader
	}
	reader.settings = result.Settings
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
	return FaultsDocument{Accepted: len(r.faults) == 0, Faults: append([]ValidationError{}, r.faults...), Settings: r.settings}
}

// JSON gives the structured document as JSON, for an agent to read.
func (r FaultsReader) JSON() ([]byte, error) {
	return json.MarshalIndent(r.Document(), "", "  ")
}

// Prose gives the faults for a person, each rendered from its structured fields, and for an accepted declaration
// its settings with the defaults filled in.
func (r FaultsReader) Prose() string {
	prose := FormatValidation(&ValidationResult{Errors: r.faults})
	if len(r.faults) == 0 && r.settings != nil {
		prose += "\n" + r.settings.Prose()
	}
	return prose
}
